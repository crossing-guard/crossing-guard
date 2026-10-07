package store

// Read side of the recorded usage calls (token-usage-analytics plan §3.7):
// grouped aggregates for the Usage page and its breakdown, the heaviest
// sessions, and one session's own and delegated calls. Every sum comes with
// how many calls stated it, so a partial figure is never shown as a whole one
// (plan invariant 3). Dimensions are framework vocabulary; their values are
// opaque and never matched here.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Usage dimensions, buckets and delegation filters (plan §3.7; session usage
// breakdown plan §5.2, P-1).
const (
	UsageDimRuntime    = "runtime"
	UsageDimModel      = "model"
	UsageDimEffort     = "effort"
	UsageDimClient     = "client"
	UsageDimDelegation = "delegation"
	// UsageDimMember names who did the work: an agent's native session id, a
	// Claude-style subagent's agent id, a child session's canonical id (its
	// alias when it has one), or the session's own id for main work.
	UsageDimMember = "member"
	// UsageDimSession is the recorded session a call belongs to.
	UsageDimSession = "session"
	// UsageDimAgentType is a subagent source's vendor-published role, or an
	// agent's run profiles; "" for main work.
	UsageDimAgentType = "agent_type"

	UsageBucketDay  = "day"
	UsageBucketWeek = "week"

	// Delegation values: the session's own calls, native children's calls,
	// and calls of Crossing Guard agent sessions working for it.
	UsageDelegationMain     = "main"
	UsageDelegationSubagent = "subagent"
	UsageDelegationAgent    = "agent"
)

// usageKeyColumns are the group-key expressions in UsageGroupKey order. A
// day is a UTC date; a week is the UTC Monday on or before the call's date.
var usageKeyColumns = []struct{ name, expr string }{
	{UsageDimRuntime, "runtime"},
	{UsageDimModel, "model"},
	{UsageDimEffort, "effort"},
	{UsageDimClient, "client"},
	{UsageDimDelegation, usageDelegationExpr},
	{UsageDimMember, usageMemberExpr},
	{UsageDimSession, "session_id"},
	{UsageDimAgentType, usageAgentTypeExpr},
}

// usageKeyCount is how many key columns lead every grouped row: the
// dimensions, then the bucket.
var usageKeyCount = len(usageKeyColumns) + 1

// The global classification (P-1): a call of a known agent session is agent
// work, a delegated call is subagent work, anything else is main. temp_agent
// is the query's agent key set (usageWith). Views of one session classify in
// Go instead (UsageWorkIndex.Classify), relative to that session (S-2).
const (
	usageIsAgentExpr    = `(runtime,session_id) IN (SELECT runtime,session_id FROM temp_agent)`
	usageDelegationExpr = `CASE WHEN ` + usageIsAgentExpr + ` THEN 'agent'
		WHEN agent<>'' OR parent_session_id<>'' THEN 'subagent' ELSE 'main' END`
	usageSourceAliasExpr = `(SELECT NULLIF(s.session_alias,'') FROM usage_source s
		WHERE s.runtime=usage_call.runtime AND s.source=usage_call.source)`
	usageMemberExpr = `CASE WHEN ` + usageIsAgentExpr + ` THEN (SELECT a.agent_id FROM temp_agent a
			WHERE a.runtime=usage_call.runtime AND a.session_id=usage_call.session_id)
		WHEN agent<>'' THEN agent
		WHEN parent_session_id<>'' THEN COALESCE(` + usageSourceAliasExpr + `,session_id)
		ELSE session_id END`
	usageAgentTypeExpr = `CASE WHEN ` + usageIsAgentExpr + ` THEN (SELECT a.profiles FROM temp_agent a
			WHERE a.runtime=usage_call.runtime AND a.session_id=usage_call.session_id)
		WHEN agent<>'' OR parent_session_id<>'' THEN COALESCE((SELECT s.role FROM usage_source s
			WHERE s.runtime=usage_call.runtime AND s.source=usage_call.source),'')
		ELSE '' END`
)

var usageBucketExprs = map[string]string{
	UsageBucketDay:  `date(at_ms/1000,'unixepoch')`,
	UsageBucketWeek: `date(at_ms/1000,'unixepoch','-6 days','weekday 1')`,
}

// UsageQuery selects calls by completion time (milliseconds; 0 = open),
// runtime and delegation, and names the dimensions to group by. Details names
// the per-group details to compute; zero means all of them. Agents is the
// agent key set the classification reads (UsageWorkIndex.Keys); without it
// no call is agent work.
type UsageQuery struct {
	FromMS, ToMS int64
	Runtime      string
	Delegation   string
	GroupBy      []string
	Bucket       string
	Details      UsageDetail
	Agents       []UsageAgentKey
}

// UsageDetail selects per-group details. Each costs a pass over the calls, so
// a caller asks only for what it shows (code red-team C-3).
type UsageDetail uint8

const (
	UsageDetailParts UsageDetail = 1 << iota
	UsageDetailOther
	UsageDetailCost
	UsageDetailReaders
	usageDetailAll = UsageDetailParts | UsageDetailOther | UsageDetailCost | UsageDetailReaders
)

// UsageGroupKey is one group's key; dimensions not grouped by are "".
type UsageGroupKey struct {
	Runtime, Model, Effort, Client, Delegation, Member, Session, AgentType, Bucket string
}

// targets are the key's scan destinations, in key-column order.
func (key *UsageGroupKey) targets() []any {
	return []any{&key.Runtime, &key.Model, &key.Effort, &key.Client, &key.Delegation, &key.Member, &key.Session,
		&key.AgentType, &key.Bucket}
}

// UsageClassSum is a class summed over the calls that stated it.
type UsageClassSum struct {
	Sum, Stated int64
}

// UsagePartSum is a labelled part of one class, summed; its JSON names are
// the canonical parts form the recorder writes.
type UsagePartSum struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Of    string `json:"of"`
	Count int64  `json:"count"`
}

type UsageOtherSum struct {
	ID, Label, Side string
	Count           int64
}

type UsageCostSum struct {
	Unit, Basis string
	Amount      float64
	Calls       int64
}

type UsageReaderCount struct {
	Reader string
	Calls  int64
}

// UsageGroup is one group's aggregates. ShareReasoning and ShareOutput sum
// over the ShareCalls that stated both; ContextSum over the ContextCalls that
// stated all three input classes. Total is every stated token (the four
// classes and other classes), the Usage page's Total. FirstAtMS and LastAtMS
// span the calls; PeakContext is the largest stated input of one call.
type UsageGroup struct {
	Key                                             UsageGroupKey
	Calls, Sessions                                 int64
	Input, CacheRead, CacheWrite, Output, Reasoning UsageClassSum
	ShareReasoning, ShareOutput, ShareCalls         int64
	ContextSum, ContextCalls                        int64
	Total                                           int64
	FirstAtMS, LastAtMS, PeakContext                int64
	Models                                          []string
	Parts                                           []UsagePartSum
	Other                                           []UsageOtherSum
	Cost                                            []UsageCostSum
	Readers                                         []UsageReaderCount
}

// ErrUsageQueryInvalid rejects an unknown dimension, bucket or filter.
var ErrUsageQueryInvalid = fmt.Errorf("usage query is invalid")

// usageScope is a validated query: its WITH clause (the agent key set), key
// expressions and WHERE clause. args are the WITH clause's, then the WHERE's,
// the order they appear in every query built from the scope.
type usageScope struct {
	with  string
	keys  []string // usageKeyCount expressions, in UsageGroupKey order
	where string
	args  []any
}

// usageAgentRow is one agent key as the temp_agent CTE reads it.
type usageAgentRow struct {
	Runtime  string `json:"r"`
	Session  string `json:"s"`
	Agent    string `json:"a"`
	Profiles string `json:"p"`
}

// usageWith binds the agent key set as a CTE. An empty set binds an empty
// array, so every classifying expression can name temp_agent.
func usageWith(agents []UsageAgentKey) (string, any, error) {
	rows := make([]usageAgentRow, 0, len(agents))
	for _, agent := range agents {
		rows = append(rows, usageAgentRow{Runtime: agent.Session.Runtime, Session: agent.Session.SessionID,
			Agent: agent.Agent, Profiles: strings.Join(agent.Profiles, ",")})
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return "", nil, err
	}
	return `WITH temp_agent(runtime,session_id,agent_id,profiles) AS (SELECT json_extract(value,'$.r'),
		json_extract(value,'$.s'),json_extract(value,'$.a'),json_extract(value,'$.p') FROM json_each(?)) `,
		string(body), nil
}

// groupList is "1,2,…,n" over the leading key columns and extra more.
func usageGroupList(extra int) string {
	parts := make([]string, 0, usageKeyCount+extra)
	for index := 1; index <= usageKeyCount+extra; index++ {
		parts = append(parts, strconv.Itoa(index))
	}
	return strings.Join(parts, ",")
}

func (q UsageQuery) scope() (usageScope, error) {
	grouped := map[string]bool{}
	for _, dimension := range q.GroupBy {
		known := false
		for _, column := range usageKeyColumns {
			known = known || column.name == dimension
		}
		if !known {
			return usageScope{}, fmt.Errorf("%w: dimension %q", ErrUsageQueryInvalid, dimension)
		}
		grouped[dimension] = true
	}
	with, agents, err := usageWith(q.Agents)
	if err != nil {
		return usageScope{}, err
	}
	scope := usageScope{with: with, args: []any{agents}}
	for _, column := range usageKeyColumns {
		if grouped[column.name] {
			scope.keys = append(scope.keys, column.expr)
		} else {
			scope.keys = append(scope.keys, "''")
		}
	}
	if q.Bucket == "" {
		scope.keys = append(scope.keys, "''")
	} else if expr, ok := usageBucketExprs[q.Bucket]; ok {
		scope.keys = append(scope.keys, expr)
	} else {
		return usageScope{}, fmt.Errorf("%w: bucket %q", ErrUsageQueryInvalid, q.Bucket)
	}
	conditions := []string{"1=1"}
	if q.FromMS > 0 {
		conditions = append(conditions, "at_ms >= ?")
		scope.args = append(scope.args, q.FromMS)
	}
	if q.ToMS > 0 {
		conditions = append(conditions, "at_ms < ?")
		scope.args = append(scope.args, q.ToMS)
	}
	if q.Runtime != "" {
		conditions = append(conditions, "runtime = ?")
		scope.args = append(scope.args, q.Runtime)
	}
	switch q.Delegation {
	case "":
	case UsageDelegationMain, UsageDelegationSubagent, UsageDelegationAgent:
		conditions = append(conditions, usageDelegationExpr+" = ?")
		scope.args = append(scope.args, q.Delegation)
	default:
		return usageScope{}, fmt.Errorf("%w: delegation %q (one of main, subagent, agent)", ErrUsageQueryInvalid,
			q.Delegation)
	}
	scope.where = strings.Join(conditions, " AND ")
	return scope, nil
}

func (s usageScope) keyList() string { return strings.Join(s.keys, ",") }

// UsageGroups aggregates the query's calls by its dimensions, largest group
// (by calls) first.
func (ix *Index) UsageGroups(q UsageQuery) ([]UsageGroup, error) {
	scope, err := q.scope()
	if err != nil {
		return nil, err
	}
	groups := map[UsageGroupKey]*UsageGroup{}
	if err := ix.usageGroupTotals(scope, groups); err != nil {
		return nil, err
	}
	details := q.Details
	if details == 0 {
		details = usageDetailAll
	}
	for _, detail := range []struct {
		flag UsageDetail
		run  func(usageScope, map[UsageGroupKey]*UsageGroup) error
	}{{UsageDetailParts, ix.usageGroupParts}, {UsageDetailOther, ix.usageGroupOther},
		{UsageDetailCost, ix.usageGroupCost}, {UsageDetailReaders, ix.usageGroupReaders}} {
		if details&detail.flag == 0 {
			continue
		}
		if err := detail.run(scope, groups); err != nil {
			return nil, err
		}
	}
	out := make([]UsageGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return usageKeyLess(out[i].Key, out[j].Key)
	})
	return out, nil
}

func usageKeyLess(a, b UsageGroupKey) bool {
	left := []string{a.Bucket, a.Runtime, a.Model, a.Effort, a.Client, a.Delegation, a.Member, a.Session, a.AgentType}
	right := []string{b.Bucket, b.Runtime, b.Model, b.Effort, b.Client, b.Delegation, b.Member, b.Session, b.AgentType}
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

// usageClassColumns sums each class and counts the calls that stated it.
const usageClassColumns = `COALESCE(SUM(input),0),COUNT(input),COALESCE(SUM(cache_read),0),COUNT(cache_read),
	COALESCE(SUM(cache_write),0),COUNT(cache_write),COALESCE(SUM(output),0),COUNT(output),
	COALESCE(SUM(reasoning),0),COUNT(reasoning)`

// usageTotalExpr is one call's Total: the four classes and other classes.
const usageTotalExpr = `COALESCE(input,0)+COALESCE(cache_read,0)+COALESCE(cache_write,0)+COALESCE(output,0)+
	CASE WHEN other='' THEN 0 ELSE (SELECT COALESCE(SUM(json_extract(o.value,'$.count')),0)
	 FROM json_each(other) AS o) END`

func (ix *Index) usageGroupTotals(scope usageScope, groups map[UsageGroupKey]*UsageGroup) error {
	rows, err := ix.db.Query(scope.with+`SELECT `+scope.keyList()+`,COUNT(*),COUNT(DISTINCT runtime||char(0)||session_id),
		`+usageClassColumns+`,
		COALESCE(SUM(CASE WHEN reasoning IS NOT NULL AND output IS NOT NULL THEN reasoning END),0),
		COALESCE(SUM(CASE WHEN reasoning IS NOT NULL AND output IS NOT NULL THEN output END),0),
		COALESCE(SUM(CASE WHEN reasoning IS NOT NULL AND output IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN input IS NOT NULL AND cache_read IS NOT NULL AND cache_write IS NOT NULL
			THEN input+cache_read+cache_write END),0),
		COALESCE(SUM(CASE WHEN input IS NOT NULL AND cache_read IS NOT NULL AND cache_write IS NOT NULL
			THEN 1 ELSE 0 END),0),
		SUM(`+usageTotalExpr+`),MIN(first_at_ms),MAX(at_ms),
		MAX(COALESCE(input,0)+COALESCE(cache_read,0)+COALESCE(cache_write,0)),
		COALESCE(GROUP_CONCAT(DISTINCT NULLIF(model,'')),'')
		FROM usage_call WHERE `+scope.where+` GROUP BY `+usageGroupList(0), scope.args...)
	if err != nil {
		return fmt.Errorf("aggregate usage calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		group := &UsageGroup{}
		var models string
		if err := rows.Scan(append(group.Key.targets(),
			&group.Calls, &group.Sessions, &group.Input.Sum, &group.Input.Stated, &group.CacheRead.Sum,
			&group.CacheRead.Stated, &group.CacheWrite.Sum, &group.CacheWrite.Stated, &group.Output.Sum,
			&group.Output.Stated, &group.Reasoning.Sum, &group.Reasoning.Stated, &group.ShareReasoning,
			&group.ShareOutput, &group.ShareCalls, &group.ContextSum, &group.ContextCalls, &group.Total,
			&group.FirstAtMS, &group.LastAtMS, &group.PeakContext, &models)...); err != nil {
			return err
		}
		if models != "" {
			group.Models = strings.Split(models, ",")
			sort.Strings(group.Models)
		}
		groups[group.Key] = group
	}
	return rows.Err()
}

// scanUsageDetail runs a detail query whose leading columns are the group
// key and hands the rest of each row to fill for its group.
func (ix *Index) scanUsageDetail(query string, scope usageScope, groups map[UsageGroupKey]*UsageGroup,
	fill func(*UsageGroup, *sql.Rows, *UsageGroupKey) error) error {
	rows, err := ix.db.Query(scope.with+query, scope.args...)
	if err != nil {
		return fmt.Errorf("aggregate usage details: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key UsageGroupKey
		group := &UsageGroup{}
		if err := fill(group, rows, &key); err != nil {
			return err
		}
		if target := groups[key]; target != nil {
			target.Parts = append(target.Parts, group.Parts...)
			target.Other = append(target.Other, group.Other...)
			target.Cost = append(target.Cost, group.Cost...)
			target.Readers = append(target.Readers, group.Readers...)
		}
	}
	return rows.Err()
}

// usageGroupParts sums parts in Go from the one parts column: json_each over
// every row cost 1.5 s at 143k calls (code red-team C-3); decoding the short
// canonical arrays here costs a fraction of that.
func (ix *Index) usageGroupParts(scope usageScope, groups map[UsageGroupKey]*UsageGroup) error {
	rows, err := ix.db.Query(scope.with+`SELECT `+scope.keyList()+`,parts FROM usage_call
		WHERE parts<>'' AND `+scope.where, scope.args...)
	if err != nil {
		return fmt.Errorf("read usage parts: %w", err)
	}
	defer rows.Close()
	type partKey struct {
		group  UsageGroupKey
		of, id string
	}
	sums := map[partKey]*UsagePartSum{}
	var order []partKey
	for rows.Next() {
		var key UsageGroupKey
		var body string
		if err := rows.Scan(append(key.targets(), &body)...); err != nil {
			return err
		}
		var parts []UsagePartSum
		if err := json.Unmarshal([]byte(body), &parts); err != nil {
			return fmt.Errorf("decode usage parts: %w", err)
		}
		for _, part := range parts {
			at := partKey{group: key, of: part.Of, id: part.ID}
			if sums[at] == nil {
				sums[at] = &UsagePartSum{Of: part.Of, ID: part.ID, Label: part.Label}
				order = append(order, at)
			}
			sums[at].Count += part.Count
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].of != order[j].of {
			return order[i].of < order[j].of
		}
		return order[i].id < order[j].id
	})
	for _, at := range order {
		if group := groups[at.group]; group != nil {
			group.Parts = append(group.Parts, *sums[at])
		}
	}
	return nil
}

func (ix *Index) usageGroupOther(scope usageScope, groups map[UsageGroupKey]*UsageGroup) error {
	query := `SELECT ` + scope.keyList() + `,json_extract(o.value,'$.id'),MIN(json_extract(o.value,'$.label')),
		MIN(json_extract(o.value,'$.side')),SUM(json_extract(o.value,'$.count'))
		FROM usage_call, json_each(usage_call.other) AS o
		WHERE usage_call.other<>'' AND ` + scope.where + ` GROUP BY ` + usageGroupList(1) +
		` ORDER BY ` + strconv.Itoa(usageKeyCount+1)
	return ix.scanUsageDetail(query, scope, groups, func(group *UsageGroup, rows *sql.Rows, key *UsageGroupKey) error {
		var other UsageOtherSum
		err := rows.Scan(append(key.targets(), &other.ID, &other.Label, &other.Side, &other.Count)...)
		group.Other = append(group.Other, other)
		return err
	})
}

func (ix *Index) usageGroupCost(scope usageScope, groups map[UsageGroupKey]*UsageGroup) error {
	query := `SELECT ` + scope.keyList() + `,cost_unit,cost_basis,SUM(cost_amount),COUNT(*)
		FROM usage_call WHERE ` + scope.where + ` AND cost_amount IS NOT NULL
		GROUP BY ` + usageGroupList(2) + ` ORDER BY ` + strconv.Itoa(usageKeyCount+1) + `,` + strconv.Itoa(usageKeyCount+2)
	return ix.scanUsageDetail(query, scope, groups, func(group *UsageGroup, rows *sql.Rows, key *UsageGroupKey) error {
		var cost UsageCostSum
		err := rows.Scan(append(key.targets(), &cost.Unit, &cost.Basis, &cost.Amount, &cost.Calls)...)
		group.Cost = append(group.Cost, cost)
		return err
	})
}

func (ix *Index) usageGroupReaders(scope usageScope, groups map[UsageGroupKey]*UsageGroup) error {
	query := `SELECT ` + scope.keyList() + `,reader,COUNT(*) FROM usage_call WHERE ` + scope.where + `
		GROUP BY ` + usageGroupList(1) + ` ORDER BY ` + strconv.Itoa(usageKeyCount+1)
	return ix.scanUsageDetail(query, scope, groups, func(group *UsageGroup, rows *sql.Rows, key *UsageGroupKey) error {
		var reader UsageReaderCount
		err := rows.Scan(append(key.targets(), &reader.Reader, &reader.Calls)...)
		group.Readers = append(group.Readers, reader)
		return err
	})
}

// Per-session queries name the session index: without table statistics the
// planner prefers the primary key's runtime prefix, which scans every call of
// the runtime once per session (code red-team C-2, C-3).

// usageCallColumns is the column list scanUsageCall reads.
const usageCallColumns = `runtime,call_id,session_id,parent_session_id,agent,source,source_born_ms,first_at_ms,
	at_ms,model,effort,client,input,cache_read,cache_write,output,reasoning,context_window,cost_amount,cost_unit,
	cost_basis,parts,other,reader`

func scanUsageCall(rows *sql.Rows) (UsageCallRecord, error) {
	var call UsageCallRecord
	var amount sql.NullFloat64
	var unit, basis sql.NullString
	err := rows.Scan(&call.Runtime, &call.CallID, &call.SessionID, &call.ParentSessionID, &call.Agent,
		&call.Source, &call.SourceBornMS, &call.FirstAtMS, &call.AtMS, &call.Model, &call.Effort, &call.Client,
		&call.Input, &call.CacheRead, &call.CacheWrite, &call.Output, &call.Reasoning, &call.ContextWindow,
		&amount, &unit, &basis, &call.Parts, &call.Other, &call.Reader)
	if amount.Valid {
		call.Cost = &UsageCost{Amount: amount.Float64, Unit: unit.String, Basis: basis.String}
	}
	return call, err
}

func (ix *Index) queryUsageCalls(query string, args ...any) ([]UsageCallRecord, error) {
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("read usage calls: %w", err)
	}
	defer rows.Close()
	var out []UsageCallRecord
	for rows.Next() {
		call, err := scanUsageCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, call)
	}
	return out, rows.Err()
}

// UsageSessionKeys returns every (runtime, session) with a recorded own call.
func (ix *Index) UsageSessionKeys() (map[[2]string]bool, error) {
	rows, err := ix.db.Query(`SELECT DISTINCT runtime,session_id FROM usage_call WHERE agent=''`)
	if err != nil {
		return nil, fmt.Errorf("list usage sessions: %w", err)
	}
	defer rows.Close()
	out := map[[2]string]bool{}
	for rows.Next() {
		var runtime, session string
		if err := rows.Scan(&runtime, &session); err != nil {
			return nil, err
		}
		out[[2]string{runtime, session}] = true
	}
	return out, rows.Err()
}

// ValidateUsageQuery reports whether a query names only known dimensions,
// buckets and filters, so a route can refuse a bad request as the caller's.
func ValidateUsageQuery(q UsageQuery) error {
	_, err := q.scope()
	return err
}
