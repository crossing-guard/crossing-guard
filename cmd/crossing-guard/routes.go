package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"crossing-guard/internal/daemon"
)

// The `routes` verb: named model routes from the terminal (team rest-of-release plan
// §5.5). The daemon owns routes; this verb asks it through the loopback route API, so a
// journey can create a route and place an agent on it with no browser.

const routesUsage = `usage:
  crossing-guard routes list
  crossing-guard routes show <id|name>
  crossing-guard routes create --name <name> --family runtime-model --runtime <runtime> --model <model> [--effort <level>]
  crossing-guard routes create --name <name> --family inference --endpoint <loopback url> --model <model>
  crossing-guard routes rename <id|name> <new name>
  crossing-guard routes delete <id|name>`

// routeDaemonTimeout bounds one request to the local daemon from these verbs.
const routeDaemonTimeout = 30 * time.Second

// cliRoute is one route as the route API lists it.
type cliRoute struct {
	RouteID    string         `json:"route_id"`
	Name       string         `json:"name"`
	Family     string         `json:"family"`
	Kind       string         `json:"kind"`
	Fields     cliRouteFields `json:"fields"`
	Local      bool           `json:"local"`
	Locality   string         `json:"locality"`
	StateToken string         `json:"state_token"`
	MigratedAt string         `json:"migrated_at,omitempty"`
	Places     []cliPlace     `json:"places"`
}

type cliRouteFields struct {
	Runtime        string     `json:"runtime,omitempty"`
	Model          string     `json:"model"`
	ThinkingEffort *cliEffort `json:"thinking_effort,omitempty"`
	Endpoint       string     `json:"endpoint,omitempty"`
}

type cliEffort struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

type cliPlace struct {
	Label string `json:"label"`
	State string `json:"state"`
}

type cliRouteDraft struct {
	RouteID string         `json:"route_id,omitempty"`
	Name    string         `json:"name"`
	Family  string         `json:"family"`
	Fields  cliRouteFields `json:"fields"`
}

func routesCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, routesUsage)
		os.Exit(2)
	}
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no local daemon to ask:", err)
		os.Exit(1)
	}
	var runErr error
	switch args[0] {
	case "list":
		runErr = routesList(loc)
	case "show":
		runErr = routesShow(loc, args[1:])
	case "create":
		runErr = routesCreate(loc, args[1:])
	case "rename":
		runErr = routesRename(loc, args[1:])
	case "delete":
		runErr = routesDelete(loc, args[1:])
	default:
		fmt.Fprintln(os.Stderr, routesUsage)
		os.Exit(2)
	}
	if errors.Is(runErr, errRoutesUsage) {
		fmt.Fprintln(os.Stderr, routesUsage)
		os.Exit(2)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

var errRoutesUsage = errors.New("usage")

func listRoutes(loc daemon.Location) ([]cliRoute, error) {
	var listed struct {
		Routes []cliRoute `json:"routes"`
	}
	if err := daemonJSON(loc, http.MethodGet, "/api/model-routes", nil, &listed); err != nil {
		return nil, fmt.Errorf("the model routes could not be read: %w", err)
	}
	return listed.Routes, nil
}

// findRoute resolves an id or a name (after trimming and case folding, the way names
// are unique) to one route.
func findRoute(loc daemon.Location, idOrName string) (cliRoute, error) {
	routes, err := listRoutes(loc)
	if err != nil {
		return cliRoute{}, err
	}
	wanted := foldRouteName(idOrName)
	for _, route := range routes {
		if route.RouteID == idOrName || foldRouteName(route.Name) == wanted {
			return route, nil
		}
	}
	return cliRoute{}, fmt.Errorf("no model route is named or has the id %q; run crossing-guard routes list", idOrName)
}

func foldRouteName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func routesList(loc daemon.Location) error {
	routes, err := listRoutes(loc)
	if err != nil {
		return err
	}
	if len(routes) == 0 {
		fmt.Println("no model routes yet; create one with crossing-guard routes create")
		return nil
	}
	for _, route := range routes {
		fmt.Printf("%s  %s  (%s, %s, used by %d)\n", route.RouteID, route.Name, route.Family, route.Locality, len(route.Places))
	}
	return nil
}

func routesShow(loc daemon.Location, args []string) error {
	if len(args) != 1 {
		return errRoutesUsage
	}
	route, err := findRoute(loc, args[0])
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(route, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func routesCreate(loc daemon.Location, args []string) error {
	draft := cliRouteDraft{}
	effort := ""
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return errRoutesUsage
		}
		value := args[index+1]
		switch args[index] {
		case "--name":
			draft.Name = value
		case "--family":
			draft.Family = value
		case "--runtime":
			draft.Fields.Runtime = value
		case "--model":
			draft.Fields.Model = value
		case "--endpoint":
			draft.Fields.Endpoint = value
		case "--effort":
			effort = value
		default:
			return errRoutesUsage
		}
		index++
	}
	if draft.Name == "" || draft.Family == "" {
		return errRoutesUsage
	}
	if effort != "" {
		draft.Fields.ThinkingEffort = &cliEffort{Kind: "level", Value: effort}
	}
	route, err := selectRoute(loc, draft)
	if err != nil {
		return err
	}
	fmt.Printf("created model route %s  %s  (%s)\n", route.RouteID, route.Name, route.Locality)
	return nil
}

func routesRename(loc daemon.Location, args []string) error {
	if len(args) != 2 {
		return errRoutesUsage
	}
	current, err := findRoute(loc, args[0])
	if err != nil {
		return err
	}
	route, err := selectRoute(loc, cliRouteDraft{RouteID: current.RouteID, Name: args[1], Family: current.Family, Fields: current.Fields})
	if err != nil {
		return err
	}
	fmt.Printf("renamed model route %s to %s; every place that uses it keeps it\n", route.RouteID, route.Name)
	return nil
}

func routesDelete(loc daemon.Location, args []string) error {
	if len(args) != 1 {
		return errRoutesUsage
	}
	current, err := findRoute(loc, args[0])
	if err != nil {
		return err
	}
	body := map[string]any{"expected_state_token": current.StateToken, "confirmed": true}
	if err := daemonJSON(loc, http.MethodDelete, "/api/model-routes/"+current.RouteID, body, nil); err != nil {
		return fmt.Errorf("delete refused: %w", err)
	}
	fmt.Printf("deleted model route %s  %s\n", current.RouteID, current.Name)
	return nil
}

// selectRoute previews a draft and, when the preview names no problem, selects it —
// the two steps the route API requires of every writer.
func selectRoute(loc daemon.Location, draft cliRouteDraft) (cliRoute, error) {
	var preview struct {
		PreviewDigest string `json:"preview_digest"`
		StateToken    string `json:"state_token"`
		Problems      []struct {
			Message string `json:"message"`
		} `json:"problems"`
	}
	if err := daemonJSON(loc, http.MethodPost, "/api/model-routes/preview", draft, &preview); err != nil {
		return cliRoute{}, fmt.Errorf("the model route was refused: %w", err)
	}
	if len(preview.Problems) > 0 {
		reasons := make([]string, 0, len(preview.Problems))
		for _, problem := range preview.Problems {
			reasons = append(reasons, problem.Message)
		}
		return cliRoute{}, errors.New("the model route was refused: " + strings.Join(reasons, " "))
	}
	request := struct {
		cliRouteDraft
		PreviewDigest      string `json:"preview_digest"`
		ExpectedStateToken string `json:"expected_state_token"`
		Confirmed          bool   `json:"confirmed"`
	}{cliRouteDraft: draft, PreviewDigest: preview.PreviewDigest, ExpectedStateToken: preview.StateToken, Confirmed: true}
	var selected struct {
		Route cliRoute `json:"route"`
	}
	if err := daemonJSON(loc, http.MethodPost, "/api/model-routes/select", request, &selected); err != nil {
		return cliRoute{}, fmt.Errorf("the model route was refused: %w", err)
	}
	return selected.Route, nil
}

// daemonJSON performs one authenticated JSON request against the located daemon. The
// daemon's locator publishes GET and POST helpers; the route API also needs DELETE and
// a binding write needs PUT, so the verbs that use those go through here. The token is
// sent as a header and never appears in an error.
func daemonJSON(loc daemon.Location, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, "http://"+loc.Addr+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+loc.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: routeDaemonTimeout}).Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return errors.New(response.Status + ": " + daemonReason(text))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// daemonReason is the daemon's own words from an error body: the typed error's message
// when the body is one, else the text.
func daemonReason(body []byte) string {
	var typed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &typed) == nil && typed.Error.Message != "" {
		return typed.Error.Code + ": " + typed.Error.Message
	}
	return strings.TrimSpace(string(body))
}
