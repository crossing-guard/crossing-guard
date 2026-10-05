package recallmcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Daemon is one located Crossing Guard daemon: authenticated GETs, plus
// PostJSON for the one write-shaped door (propose_memory). The contract is
// part of the type: reads go through GetJSON*; PostJSON exists solely for
// the proposal route, whose consent the daemon checks server-side — the
// interface staying narrow is what makes that check trustworthy (RT-6).
// GetJSON runs under the daemon client's own short timeout (the config read);
// GetJSONContext under the caller's (the tool routes).
type Daemon interface {
	Addr() string
	GetJSON(path string, out any) error
	GetJSONContext(ctx context.Context, path string, out any) error
	PostJSON(path string, body, out any) error
}

// Locator finds the daemon of record. It is called again after a failed call,
// so a daemon started or moved after the session began is found.
type Locator func() (Daemon, error)

// Config is the daemon.json recall section the tools read.
type Config struct {
	RequestTimeoutMS     int `json:"request_timeout_ms"`
	MaxResultBytes       int `json:"max_result_bytes"`
	MemoryBodyMaxBytes   int `json:"memory_body_max_bytes"`
	MemorySearchLimitMax int `json:"memory_search_limit_max"`
}

type client struct {
	locate Locator
	mu     sync.Mutex
	daemon Daemon
	config *Config
}

// errUnavailable wraps every failure to reach the daemon, so a tool reports
// "the daemon is unavailable", never an empty answer.
type errUnavailable struct {
	addr string
	err  error
}

func (e *errUnavailable) Error() string {
	if e.addr == "" {
		return "the Crossing Guard daemon could not be located: " + e.err.Error()
	}
	return fmt.Sprintf("the Crossing Guard daemon at %s did not answer: %v", e.addr, e.err)
}

func (c *client) current() (Daemon, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.daemon != nil {
		return c.daemon, nil
	}
	daemon, err := c.locate()
	if err != nil {
		return nil, &errUnavailable{err: err}
	}
	c.daemon = daemon
	return daemon, nil
}

// forget drops the located daemon and its config so the next call re-locates.
func (c *client) forget() {
	c.mu.Lock()
	c.daemon, c.config = nil, nil
	c.mu.Unlock()
}

// settings reads the recall section once per located daemon.
func (c *client) settings(ctx context.Context) (Config, error) {
	c.mu.Lock()
	cached := c.config
	c.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	daemon, err := c.current()
	if err != nil {
		return Config{}, err
	}
	var body struct {
		Config struct {
			Recall Config `json:"recall"`
		} `json:"config"`
	}
	if err := daemon.GetJSON("/api/console/config", &body); err != nil {
		c.forget()
		return Config{}, &errUnavailable{addr: daemon.Addr(), err: err}
	}
	recall := body.Config.Recall
	if recall.RequestTimeoutMS <= 0 || recall.MaxResultBytes <= 0 {
		return Config{}, errors.New("the daemon's console configuration has no recall section; it predates these tools")
	}
	c.mu.Lock()
	c.config = &recall
	c.mu.Unlock()
	return recall, nil
}

// get performs one route call under the configured request timeout.
func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	config, err := c.settings(ctx)
	if err != nil {
		return err
	}
	daemon, err := c.current()
	if err != nil {
		return err
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(config.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	if err := daemon.GetJSONContext(callCtx, path, out); err != nil {
		var transport *url.Error
		if errors.As(err, &transport) {
			// Not reached (stopped, moved, timed out): locate afresh next call.
			c.forget()
			return &errUnavailable{addr: daemon.Addr(), err: err}
		}
		if text := err.Error(); strings.Contains(text, " 401 ") || strings.Contains(text, " 403 ") {
			// A token the daemon no longer accepts: another daemon or data
			// directory. Locate afresh so the next call reads the current token.
			c.forget()
		}
		return fmt.Errorf("the Crossing Guard daemon refused the request: %w", err)
	}
	return nil
}

// post performs one write-shaped route call — the propose door only, and only
// after the caller checked the daemon's consent flag (the route re-checks;
// two keys, one lock — the store is what actually enforces pending).
func (c *client) post(_ context.Context, path string, body, out any) error {
	daemon, err := c.current()
	if err != nil {
		return err
	}
	return daemon.PostJSON(path, body, out)
}
