package pgstoretest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type ReportPlanContract struct {
	Relations []string
	Indexes   map[string]ReportIndex
	Cap       int
}

type ReportIndex struct {
	Unique  bool
	Columns []string
}

type ReportPlanScan struct {
	Relation string
	Alias    string
	Node     string
	Index    string
	Scope    string
	Class    string
	Loops    float64
	PerLoop  float64
	Work     float64
	Bound    float64
	Probe    bool
	rounding float64
}

type ReportPlanResult struct {
	Scans    []ReportPlanScan
	Problems []string
	Work     float64
	Ceiling  float64
}

func (r ReportPlanResult) Probes() int {
	count := 0
	for _, scan := range r.Scans {
		if scan.Probe {
			count++
		}
	}
	return count
}

func (r ReportPlanResult) Discovery() float64 {
	work := 0.0
	for _, scan := range r.Scans {
		if !scan.Probe {
			work += scan.Work
		}
	}
	return work
}

type reportPlanNode struct {
	Node        string           `json:"Node Type"`
	Strategy    string           `json:"Strategy"`
	Join        string           `json:"Join Type"`
	InnerUnique bool             `json:"Inner Unique"`
	Parent      string           `json:"Parent Relationship"`
	Subplan     string           `json:"Subplan Name"`
	Relation    string           `json:"Relation Name"`
	Alias       string           `json:"Alias"`
	Index       string           `json:"Index Name"`
	CTE         string           `json:"CTE Name"`
	Function    string           `json:"Function Call"`
	IndexCond   string           `json:"Index Cond"`
	Recheck     string           `json:"Recheck Cond"`
	Filter      string           `json:"Filter"`
	JoinFilter  string           `json:"Join Filter"`
	SortSpace   string           `json:"Sort Space Type"`
	Storage     string           `json:"Storage"`
	HashBatches float64          `json:"Hash Batches"`
	DiskUsage   float64          `json:"Disk Usage"`
	Loops       float64          `json:"Actual Loops"`
	Plans       []reportPlanNode `json:"Plans"`
	raw         map[string]any
	rows        SearchPlanNode
}

func (p *reportPlanNode) UnmarshalJSON(data []byte) error {
	type node reportPlanNode
	var decoded node
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = reportPlanNode(decoded)
	if err := json.Unmarshal(data, &p.raw); err != nil {
		return err
	}
	return json.Unmarshal(data, &p.rows)
}

func (p reportPlanNode) examined() float64 {
	own := p.rows
	own.Plans = nil
	return own.examined()
}

func (p reportPlanNode) lowerExamined() float64 {
	perLoop := p.perLoop()
	if p.Loops > 1 {
		perLoop = max(0, perLoop-p.rows.rowsRounding-p.rows.removedRounding-p.rows.indexRemovedRounding)
	}
	return perLoop * p.Loops
}

func (p reportPlanNode) perLoop() float64 {
	return p.rows.Rows + p.rows.Removed + p.rows.IndexRemoved
}

func (p reportPlanNode) texts() []string {
	out := []string{}
	for key, value := range p.raw {
		if key == "Plans" {
			continue
		}
		switch value := value.(type) {
		case string:
			out = append(out, value)
		case []any:
			for _, item := range value {
				if text, ok := item.(string); ok {
					out = append(out, text)
				}
			}
		}
	}
	return out
}

var (
	reportArrayLiteral = regexp.MustCompile(`'\{([^']*)\}'::[a-z ]+\[\]`)
	reportDepthFilter  = regexp.MustCompile(`\(\w+\.depth <= (\d+)\)`)
	reportIdentifier   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reportPlanAlias    = regexp.MustCompile(`^(.+)_[0-9]+$`)
)

type reportLimit struct {
	value int
	valid bool
}

type reportRelation struct {
	name  string
	alias string
}

type reportBlock struct {
	parent    int
	cte       string
	limit     *reportLimit
	relations map[reportRelation]bool
	children  []int
}

type reportStatement struct {
	blocks   []reportBlock
	ctes     map[string]int
	problems []string
}

var reportAliasStop = map[string]bool{"WHERE": true, "JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true, "CROSS": true, "FULL": true, "OUTER": true, "NATURAL": true, "ON": true, "USING": true, "GROUP": true, "ORDER": true, "LIMIT": true, "OFFSET": true, "UNION": true, "EXCEPT": true, "INTERSECT": true, "WINDOW": true, "HAVING": true, "FOR": true, "RETURNING": true, "SET": true, "AND": true, "OR": true, "WITH": true, "SELECT": true, "FETCH": true, "TABLESAMPLE": true}

var reportClauseEnd = map[string]bool{"WHERE": true, "GROUP": true, "ORDER": true, "LIMIT": true, "OFFSET": true, "HAVING": true, "UNION": true, "EXCEPT": true, "INTERSECT": true, "WINDOW": true, "FETCH": true, "FOR": true}

func parseReportStatement(statement string, args []any, limitCap int) reportStatement {
	parsed := reportStatement{blocks: []reportBlock{{parent: -1, relations: map[reportRelation]bool{}}}, ctes: map[string]int{}}
	tokens := reportTokens(statement)
	stack := []int{0}
	inFrom := map[int]bool{}
	pending := ""
	at := func(index int) string {
		if index < 0 || index >= len(tokens) {
			return ""
		}
		return tokens[index]
	}
	current := func() int {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i] >= 0 {
				return stack[i]
			}
		}
		return 0
	}
	fromItem := func(index int) {
		block := current()
		next := index + 1
		for strings.EqualFold(at(next), "LATERAL") || strings.EqualFold(at(next), "ONLY") {
			next++
		}
		name := at(next)
		if name == "(" || name == "" || !reportIdentifier.MatchString(strings.ReplaceAll(name, ".", "_")) {
			return
		}
		next++
		relation := strings.ToLower(name[strings.LastIndexByte(name, '.')+1:])
		if at(next) == "(" {
			relation = ""
			depth := 0
			for ; next < len(tokens); next++ {
				if tokens[next] == "(" {
					depth++
				} else if tokens[next] == ")" {
					depth--
					if depth == 0 {
						next++
						break
					}
				}
			}
			if strings.EqualFold(at(next), "WITH") && strings.EqualFold(at(next+1), "ORDINALITY") {
				next += 2
			}
		}
		if strings.EqualFold(at(next), "AS") {
			next++
		}
		alias := relation
		if candidate := at(next); reportIdentifier.MatchString(candidate) && !reportAliasStop[strings.ToUpper(candidate)] {
			alias = strings.ToLower(candidate)
		}
		if alias == "" {
			parsed.problems = append(parsed.problems, "statement has a function source without an alias")
			return
		}
		parsed.blocks[block].relations[reportRelation{name: relation, alias: alias}] = true
	}
	for index, token := range tokens {
		upper := strings.ToUpper(token)
		switch {
		case token == "(":
			next := strings.ToUpper(at(index + 1))
			switch {
			case pending != "":
				if _, repeated := parsed.ctes[pending]; repeated {
					parsed.problems = append(parsed.problems, "statement repeats common table expression "+pending)
				}
				parsed.blocks = append(parsed.blocks, reportBlock{parent: current(), cte: pending, relations: map[reportRelation]bool{}})
				parsed.ctes[pending] = len(parsed.blocks) - 1
				pending = ""
			case next == "SELECT" || next == "WITH" || next == "VALUES":
				parsed.blocks = append(parsed.blocks, reportBlock{parent: current(), relations: map[reportRelation]bool{}})
			default:
				stack = append(stack, -1)
				continue
			}
			child := len(parsed.blocks) - 1
			parent := parsed.blocks[child].parent
			parsed.blocks[parent].children = append(parsed.blocks[parent].children, child)
			stack = append(stack, child)
			continue
		case token == ")":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		case upper == "AS" && index > 1 && reportIdentifier.MatchString(at(index-1)) && (at(index-2) == "," || strings.EqualFold(at(index-2), "WITH") || strings.EqualFold(at(index-2), "RECURSIVE")):
			next := index + 1
			for strings.EqualFold(at(next), "NOT") || strings.EqualFold(at(next), "MATERIALIZED") {
				next++
			}
			if at(next) == "(" {
				pending = strings.ToLower(at(index - 1))
			}
			continue
		case upper == "FROM" || upper == "JOIN":
			if stack[len(stack)-1] >= 0 {
				inFrom[current()] = true
				fromItem(index)
			}
			continue
		case token == ",":
			if top := stack[len(stack)-1]; top >= 0 && inFrom[top] {
				fromItem(index)
			}
			continue
		case reportClauseEnd[upper]:
			if top := stack[len(stack)-1]; top >= 0 {
				inFrom[top] = false
			}
			if upper == "FETCH" {
				parsed.problems = append(parsed.problems, "statement uses FETCH instead of a validated LIMIT")
				continue
			}
			if upper != "LIMIT" && upper != "OFFSET" {
				continue
			}
		default:
			continue
		}
		value := at(index + 1)
		resolved := value
		if strings.HasPrefix(value, "$") {
			position, err := strconv.Atoi(value[1:])
			if err != nil || position < 1 || position > len(args) {
				parsed.problems = append(parsed.problems, fmt.Sprintf("statement %s names a missing argument %s", upper, value))
				resolved = ""
			} else {
				resolved = fmt.Sprint(args[position-1])
			}
		}
		number, err := strconv.Atoi(resolved)
		if upper == "OFFSET" {
			if err != nil || number != 0 {
				parsed.problems = append(parsed.problems, "statement uses OFFSET "+value)
			}
			continue
		}
		limit := reportLimit{value: number, valid: err == nil && number >= 1 && number <= limitCap+1}
		if !limit.valid {
			parsed.problems = append(parsed.problems, fmt.Sprintf("statement LIMIT %s is not a validated count between 1 and %d", value, limitCap+1))
		}
		block := current()
		if parsed.blocks[block].limit != nil {
			parsed.problems = append(parsed.problems, "statement block has more than one LIMIT")
		}
		parsed.blocks[block].limit = &limit
	}
	return parsed
}

func (s reportStatement) closure(block int, planned map[string]bool, into map[reportRelation]bool) {
	for relation := range s.blocks[block].relations {
		into[relation] = true
	}
	for _, child := range s.blocks[block].children {
		if s.blocks[child].limit == nil && (s.blocks[child].cte == "" || !planned[s.blocks[child].cte]) {
			s.closure(child, planned, into)
		}
	}
}

func (s reportStatement) scope(block int, planned map[string]bool) (string, bool) {
	for ; block > 0; block = s.blocks[block].parent {
		if name := s.blocks[block].cte; name != "" {
			return name, !planned[name]
		}
	}
	return "", false
}

func reportTokens(statement string) []string {
	tokens := []string{}
	for i := 0; i < len(statement); {
		c := statement[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(statement) && statement[i+1] == '-':
			for i < len(statement) && statement[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(statement) && statement[i+1] == '*':
			level := 0
			for i < len(statement) {
				if strings.HasPrefix(statement[i:], "/*") {
					level++
					i += 2
				} else if strings.HasPrefix(statement[i:], "*/") {
					level--
					i += 2
					if level == 0 {
						break
					}
				} else {
					i++
				}
			}
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(statement) {
				if statement[j] == c {
					if j+1 < len(statement) && statement[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			tokens = append(tokens, "literal")
			i = j + 1
		case c == '$' && i+1 < len(statement) && (statement[i+1] == '$' || statement[i+1] >= 'A' && statement[i+1] <= 'z' && statement[i+1] != '_' || statement[i+1] == '_'):
			end := strings.IndexByte(statement[i+1:], '$')
			if end < 0 {
				tokens = append(tokens, statement[i:])
				i = len(statement)
				continue
			}
			tag := statement[i : i+end+2]
			close := strings.Index(statement[i+len(tag):], tag)
			if close < 0 {
				i = len(statement)
				continue
			}
			tokens = append(tokens, "literal")
			i += len(tag) + close + len(tag)
		case c == '(' || c == ')' || c == ',' || c == ';':
			tokens = append(tokens, string(c))
			i++
		default:
			j := i
			for j < len(statement) && !strings.ContainsRune(" \t\n\r(),;'\"", rune(statement[j])) && !strings.HasPrefix(statement[j:], "--") && !strings.HasPrefix(statement[j:], "/*") {
				j++
			}
			if j == i {
				j++
			}
			tokens = append(tokens, statement[i:j])
			i = j
		}
	}
	return tokens
}

type reportAnalysis struct {
	contract  ReportPlanContract
	statement reportStatement
	ctes      map[string]float64
	planCTEs  map[string]bool
	worktable float64
	result    ReportPlanResult
}

type reportContext struct {
	scope   string
	loops   float64
	cap     float64
	top     bool
	perLoop bool
}

func CheckReportPlan(raw []byte, statement string, args []any, contract ReportPlanContract) (ReportPlanResult, error) {
	var plans []struct{ Plan reportPlanNode }
	if err := json.Unmarshal(raw, &plans); err != nil {
		return ReportPlanResult{}, fmt.Errorf("decode report plan: %w", err)
	}
	if len(plans) != 1 {
		return ReportPlanResult{}, fmt.Errorf("decode report plan: %d plans", len(plans))
	}
	if contract.Cap < 1 {
		return ReportPlanResult{}, errors.New("report plan contract needs a candidate cap")
	}
	a := &reportAnalysis{contract: contract, statement: parseReportStatement(statement, args, contract.Cap), ctes: map[string]float64{}, planCTEs: map[string]bool{}}
	a.result.Problems = append(a.result.Problems, a.statement.problems...)
	var collect func(reportPlanNode)
	collect = func(node reportPlanNode) {
		if name, ok := strings.CutPrefix(node.Subplan, "CTE "); ok {
			a.planCTEs[strings.ToLower(name)] = true
		}
		for _, child := range node.Plans {
			collect(child)
		}
	}
	collect(plans[0].Plan)
	a.visit(plans[0].Plan, reportContext{loops: 1, cap: math.Inf(1), top: true})
	for _, scan := range a.result.Scans {
		a.result.Work += scan.Work
		a.result.Ceiling += scan.Bound
	}
	return a.result, nil
}

func reportPlanRelations(node reportPlanNode, into map[reportRelation]bool) {
	for _, child := range node.Plans {
		if child.Parent == "SubPlan" || child.Parent == "InitPlan" || child.Node == "Limit" {
			continue
		}
		name := strings.ToLower(child.Relation)
		if child.CTE != "" {
			name = strings.ToLower(child.CTE)
		}
		if alias := strings.ToLower(child.Alias); alias != "" && (name != "" || child.Node == "Function Scan" || child.Node == "Values Scan") {
			into[reportRelation{name: name, alias: alias}] = true
		}
		reportPlanRelations(child, into)
	}
}

func reportHasRelation(relations map[reportRelation]bool, planned reportRelation) bool {
	if relations[planned] {
		return true
	}
	if match := reportPlanAlias.FindStringSubmatch(planned.alias); match != nil {
		return relations[reportRelation{name: planned.name, alias: match[1]}]
	}
	return false
}

func (a *reportAnalysis) limitCount(node reportPlanNode, ctx reportContext) float64 {
	planned := map[reportRelation]bool{}
	reportPlanRelations(node, planned)
	describe := make([]string, 0, len(planned))
	for relation := range planned {
		describe = append(describe, relation.name+" "+relation.alias)
	}
	slices.Sort(describe)
	if len(planned) == 0 {
		a.problem("Limit in scope %q reads no named relation to bind to a LIMIT clause", ctx.scope)
		return math.Inf(1)
	}
	values := map[int]bool{}
	for index, block := range a.statement.blocks {
		if block.limit == nil {
			continue
		}
		if scope, inlined := a.statement.scope(index, a.planCTEs); !inlined && scope != ctx.scope {
			continue
		}
		closure := map[reportRelation]bool{}
		a.statement.closure(index, a.planCTEs, closure)
		direct, inside := false, true
		for relation := range planned {
			direct = direct || reportHasRelation(block.relations, relation)
			inside = inside && reportHasRelation(closure, relation)
		}
		if !direct || !inside {
			continue
		}
		if !block.limit.valid {
			return math.Inf(1)
		}
		values[block.limit.value] = true
	}
	switch len(values) {
	case 0:
		a.problem("Limit over %s in scope %q binds to no LIMIT clause", strings.Join(describe, ","), ctx.scope)
		return math.Inf(1)
	case 1:
		for value := range values {
			return float64(value)
		}
	}
	a.problem("Limit over %s in scope %q binds ambiguously to LIMIT clauses with different counts", strings.Join(describe, ","), ctx.scope)
	return math.Inf(1)
}

func (a *reportAnalysis) problem(format string, args ...any) {
	a.result.Problems = append(a.result.Problems, fmt.Sprintf(format, args...))
}

func reportChild(node reportPlanNode, relationship string) (reportPlanNode, bool) {
	for _, child := range node.Plans {
		if child.Parent == relationship {
			return child, true
		}
	}
	return reportPlanNode{}, false
}

func (a *reportAnalysis) visit(node reportPlanNode, ctx reportContext) float64 {
	if node.SortSpace == "Disk" || node.Storage == "Disk" || node.HashBatches > 1 || node.DiskUsage > 0 {
		a.problem("%s in scope %q spilled to disk", node.Node, ctx.scope)
	}
	for _, child := range node.Plans {
		if name, ok := strings.CutPrefix(child.Subplan, "CTE "); ok {
			name = strings.ToLower(name)
			a.ctes[name] = a.visit(child, reportContext{scope: name, loops: ctx.loops, cap: math.Inf(1), top: true}) / ctx.loops
			if math.IsInf(ctx.loops, 1) {
				a.ctes[name] = math.Inf(1)
			}
		}
	}
	inf := math.Inf(1)
	streaming := func(child reportPlanNode, cap float64) float64 {
		return a.visit(child, reportContext{scope: ctx.scope, loops: ctx.loops, cap: cap, top: ctx.top && node.Node != "Limit", perLoop: ctx.perLoop})
	}
	blocked := func(child reportPlanNode) float64 {
		return a.visit(child, reportContext{scope: ctx.scope, loops: ctx.loops, cap: inf, perLoop: ctx.perLoop})
	}
	ordinary := []reportPlanNode{}
	for _, child := range node.Plans {
		if child.Parent != "SubPlan" && child.Parent != "InitPlan" {
			ordinary = append(ordinary, child)
		}
	}
	bound := inf
	input := ctx.loops
	switch node.Node {
	case "Limit":
		count := a.limitCount(node, ctx)
		limited := min(ctx.cap, count*ctx.loops)
		bound = limited
		for _, child := range ordinary {
			bound = min(limited, streaming(child, limited))
		}
		input = bound
	case "Result", "Subquery Scan", "Materialize", "Memoize":
		cap := ctx.cap
		if node.Filter != "" {
			cap = inf
		}
		if len(ordinary) == 0 {
			bound = math.Ceil(node.rows.Rows+node.rows.rowsRounding) * ctx.loops
		}
		for _, child := range ordinary {
			bound = min(ctx.cap, streaming(child, cap))
			input = bound
		}
	case "Append", "Merge Append":
		bound = 0
		for _, child := range ordinary {
			bound += a.visit(child, reportContext{scope: ctx.scope, loops: ctx.loops, cap: ctx.cap, perLoop: ctx.perLoop})
		}
		bound = min(bound, ctx.cap)
		input = bound
	case "Nested Loop", "Hash Join", "Merge Join":
		outer, _ := reportChild(node, "Outer")
		inner, _ := reportChild(node, "Inner")
		outerCap := inf
		if node.Join == "Left" && node.Node != "Merge Join" {
			outerCap = ctx.cap
		}
		outerBound := a.visit(outer, reportContext{scope: ctx.scope, loops: ctx.loops, cap: outerCap, perLoop: ctx.perLoop})
		var innerBound float64
		if node.Node == "Nested Loop" {
			innerCap := inf
			if node.JoinFilter == "" && (node.Join == "Inner" || node.Join == "Left") {
				innerCap = ctx.cap
			}
			if node.JoinFilter == "" && (node.Join == "Semi" || node.Join == "Anti" || node.InnerUnique) {
				innerCap = min(innerCap, outerBound)
			}
			innerLoops := outerBound
			if inner.Node == "Materialize" {
				innerLoops = ctx.loops
			}
			innerBound = a.visit(inner, reportContext{scope: ctx.scope, loops: innerLoops, cap: innerCap, perLoop: true})
		} else {
			innerBound = blocked(inner)
		}
		switch {
		case node.Join == "Semi" || node.Join == "Anti":
			bound = outerBound
		case node.InnerUnique:
			bound = outerBound
		case node.Node == "Nested Loop":
			bound = innerBound
			if node.Join == "Left" {
				bound += outerBound
			}
		default:
			bound = outerBound * innerBound
			if node.Join == "Left" || node.Join == "Right" || node.Join == "Full" {
				bound += outerBound + innerBound
			}
		}
		bound = min(bound, ctx.cap)
		input = outerBound + innerBound
	case "Aggregate":
		childBound := 0.0
		for _, child := range ordinary {
			childBound += blocked(child)
		}
		bound = childBound
		if node.Strategy == "Plain" {
			bound = ctx.loops
		}
		input = childBound
	case "Incremental Sort":
		bound = 0
		for _, child := range ordinary {
			bound += streaming(child, ctx.cap)
		}
		input = bound
	case "Sort", "Unique", "Hash", "WindowAgg", "SetOp", "Gather", "Gather Merge", "LockRows":
		bound = 0
		for _, child := range ordinary {
			bound += blocked(child)
		}
		input = bound
	case "Recursive Union":
		if len(ordinary) == 2 {
			seed := blocked(ordinary[0])
			previous := a.worktable
			a.worktable = seed
			step := blocked(ordinary[1])
			a.worktable = previous
			depth := inf
			var find func(reportPlanNode)
			find = func(n reportPlanNode) {
				if n.Node == "WorkTable Scan" {
					if match := reportDepthFilter.FindStringSubmatch(n.Filter); match != nil {
						if value, err := strconv.Atoi(match[1]); err == nil {
							depth = float64(value + 1)
						}
					}
				}
				for _, child := range n.Plans {
					find(child)
				}
			}
			find(ordinary[1])
			if step <= seed {
				bound = seed * depth
			}
		}
		input = bound
	case "WorkTable Scan":
		bound = a.worktable * ctx.loops
	case "CTE Scan":
		value, ok := a.ctes[strings.ToLower(node.CTE)]
		if !ok {
			value = inf
		}
		bound = min(ctx.cap, value*ctx.loops)
		input = bound
	case "Function Scan":
		if strings.HasPrefix(node.Function, "unnest(") {
			bound = float64(a.contract.Cap) * ctx.loops
		}
		input = bound
	case "Values Scan":
		bound = math.Ceil(node.rows.Rows+node.rows.rowsRounding) * ctx.loops
	case "ProjectSet":
		for _, child := range ordinary {
			blocked(child)
		}
	default:
		if node.Relation != "" {
			bound, input = a.scan(node, ctx)
			break
		}
		for _, child := range ordinary {
			blocked(child)
		}
	}
	a.subplans(node, ctx, max(input, bound, ctx.loops))
	return bound
}

func (a *reportAnalysis) subplans(node reportPlanNode, ctx reportContext, evaluations float64) {
	texts := strings.Join(node.texts(), "\n")
	for _, child := range node.Plans {
		if (child.Parent != "SubPlan" && child.Parent != "InitPlan") || strings.HasPrefix(child.Subplan, "CTE ") {
			continue
		}
		loops, cap := evaluations, math.Inf(1)
		if child.Parent == "InitPlan" {
			loops = ctx.loops
		}
		switch {
		case strings.Contains(texts, "hashed "+child.Subplan+")"):
			loops = ctx.loops
		case strings.Contains(texts, "EXISTS("+child.Subplan+")"):
			cap = loops
		case strings.Contains(texts, "("+child.Subplan+")") && !strings.Contains(texts, "ARRAY("+child.Subplan+")") && !strings.Contains(texts, "(ANY") && !strings.Contains(texts, "(ALL"):
			cap = 2 * loops
		}
		a.visit(child, reportContext{scope: ctx.scope, loops: loops, cap: cap, perLoop: true})
	}
}

func (a *reportAnalysis) ownConditions(node reportPlanNode) []string {
	conditions := []string{node.IndexCond, node.Recheck}
	if node.Node == "Bitmap Heap Scan" {
		var visit func(reportPlanNode)
		visit = func(child reportPlanNode) {
			if child.Node != "Bitmap Index Scan" && child.Node != "BitmapAnd" && child.Node != "BitmapOr" {
				return
			}
			conditions = append(conditions, child.IndexCond)
			for _, grandchild := range child.Plans {
				visit(grandchild)
			}
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	return conditions
}

func (a *reportAnalysis) keyed(alias, index, condition string, loops float64) float64 {
	info, ok := a.contract.Indexes[index]
	if !ok || !info.Unique || condition == "" {
		return math.Inf(1)
	}
	perLoop := 1.0
	for _, column := range info.Columns {
		qualified := alias + "." + column
		if strings.Contains(condition, "("+qualified+" = ANY (") {
			perLoop = float64(a.contract.Cap)
			continue
		}
		if !strings.Contains(condition, "("+qualified+" = ") {
			return math.Inf(1)
		}
	}
	return perLoop * loops
}

func (a *reportAnalysis) bitmapEntries(alias string, node reportPlanNode, loops float64) float64 {
	switch node.Node {
	case "Bitmap Index Scan":
		return a.keyed(alias, node.Index, node.IndexCond, loops)
	case "BitmapOr":
		total := 0.0
		for _, child := range node.Plans {
			total += a.bitmapEntries(alias, child, loops)
		}
		return total
	case "BitmapAnd":
		least := math.Inf(1)
		for _, child := range node.Plans {
			least = min(least, a.bitmapEntries(alias, child, loops))
		}
		return least
	}
	return math.Inf(1)
}

func (a *reportAnalysis) scan(node reportPlanNode, ctx reportContext) (float64, float64) {
	hot := slices.Contains(a.contract.Relations, node.Relation)
	alias := node.Alias
	if alias == "" {
		alias = node.Relation
	}
	class, examined := "unbounded", math.Inf(1)
	switch node.Node {
	case "Index Scan", "Index Only Scan":
		if bound := a.keyed(alias, node.Index, node.IndexCond, ctx.loops); !math.IsInf(bound, 1) {
			class, examined = "keyed", bound
		} else if !reportResidualFilter(alias, node.Filter) && !math.IsInf(ctx.cap, 1) {
			class, examined = "limited", ctx.cap
		}
	case "Bitmap Heap Scan":
		for _, child := range node.Plans {
			if bound := a.bitmapEntries(alias, child, ctx.loops); !math.IsInf(bound, 1) {
				class, examined = "keyed", bound
			}
		}
	}
	emitted := min(examined, ctx.cap)
	if !hot {
		return emitted, examined
	}
	scan := ReportPlanScan{Relation: node.Relation, Alias: alias, Node: node.Node, Index: node.Index, Scope: ctx.scope, Class: class, Loops: node.Loops, PerLoop: node.perLoop(), Work: node.examined(), Bound: examined, Probe: ctx.perLoop || node.Loops > 1, rounding: max(node.rows.rowsRounding, node.rows.removedRounding)}
	if node.Node == "Bitmap Heap Scan" {
		entries := 0.0
		var count func(reportPlanNode)
		count = func(child reportPlanNode) {
			if child.Node == "Bitmap Index Scan" {
				entries += child.examined()
			}
			if child.Relation == "" {
				for _, grandchild := range child.Plans {
					count(grandchild)
				}
			}
		}
		for _, child := range node.Plans {
			count(child)
		}
		scan.Work = max(scan.Work, entries)
	}
	if node.Loops > 0 {
		a.result.Scans = append(a.result.Scans, scan)
	}
	describe := fmt.Sprintf("%s %s (%s) in scope %q", node.Relation, alias, node.Node, ctx.scope)
	tenant := false
	for _, condition := range a.ownConditions(node) {
		tenant = tenant || strings.Contains(condition, "("+alias+".organization_id = ")
	}
	switch {
	case strings.Contains(node.Node, "Seq Scan") || node.Index == "" && node.Node != "Bitmap Heap Scan":
		a.problem("%s is not an index scan", describe)
	case !tenant:
		a.problem("%s has no tenant equality on its own index condition", describe)
	}
	if math.IsInf(examined, 1) {
		reason := "no keyed lookup, terminating limit or EXISTS bound reaches it"
		switch {
		case math.IsInf(ctx.loops, 1):
			reason = "its loops are not bounded by a proven driver"
		case node.Node == "Bitmap Heap Scan":
			reason = "its bitmap is built eagerly from an unkeyed index range"
		case reportResidualFilter(alias, node.Filter) && !math.IsInf(ctx.cap, 1):
			reason = "its residual filter can reject arbitrarily many rows before the limit"
		}
		a.problem("%s has no proven bound: %s", describe, reason)
	} else if lower := node.lowerExamined(); lower > examined*(1+1e-9)+1e-9 {
		a.problem("%s examined at least %.2f rows above its proven bound %.0f", describe, lower, examined)
	}
	if class != "unbounded" && node.Loops > 0 && scan.PerLoop > math.Max(1, examined/math.Max(ctx.loops, 1)) {
		a.problem("%s examined %.2f rows per loop above its %s allowance", describe, scan.PerLoop, class)
	}
	if elements := reportArrayElements(node.IndexCond); class == "keyed" && elements >= 0 && scan.PerLoop > float64(elements) {
		a.problem("%s examined %.2f rows per loop above its %d keys", describe, scan.PerLoop, elements)
	}
	return emitted, examined
}

func reportArrayElements(condition string) int {
	match := reportArrayLiteral.FindStringSubmatch(condition)
	if match == nil || !strings.Contains(condition, "= ANY (") {
		return -1
	}
	if match[1] == "" {
		return 0
	}
	return strings.Count(match[1], ",") + 1
}

func reportResidualFilter(alias, filter string) bool {
	tenant := "(" + alias + ".organization_id = NULLIF(current_setting('detent.organization_id'::text, true), ''::text))"
	return strings.TrimSpace(strings.ReplaceAll(filter, tenant, "")) != ""
}

const reportIndexesQuery = `SELECT c.relname,i.indisunique,string_agg(a.attname::text,',' ORDER BY k.ord)
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum,ord)
JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
WHERE n.nspname=current_schema() AND i.indexprs IS NULL AND k.ord<=i.indnkeyatts
GROUP BY c.relname,i.indisunique`

func LoadReportIndexes(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string]ReportIndex, error) {
	rows, err := db.QueryContext(ctx, reportIndexesQuery)
	if err != nil {
		return nil, fmt.Errorf("read report indexes: %w", err)
	}
	defer rows.Close()
	indexes := map[string]ReportIndex{}
	for rows.Next() {
		var name string
		var index ReportIndex
		var columns string
		if err := rows.Scan(&name, &index.Unique, &columns); err != nil {
			return nil, fmt.Errorf("scan report index: %w", err)
		}
		index.Columns = strings.Split(columns, ",")
		indexes[name] = index
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read report indexes: %w", err)
	}
	return indexes, nil
}

type ReportRequestBudget struct {
	Overhead  int
	Probes    int
	Allowance int
	Branches  int
}

func (b ReportRequestBudget) Ceiling(candidates int) float64 {
	return float64(b.Overhead) + float64(b.Probes*b.Allowance+b.Branches)*float64(candidates+1)
}

func ReportRequestProblems(results []ReportPlanResult, budget ReportRequestBudget, candidates, limitCap int) []string {
	problems := []string{}
	probes, work, ceiling, rounding := 0, 0.0, 0.0, 0.0
	for _, result := range results {
		probes += result.Probes()
		work += result.Work
		ceiling += result.Ceiling
		for _, scan := range result.Scans {
			if scan.Probe {
				rounding = max(rounding, scan.rounding)
			}
		}
	}
	if probes > budget.Probes {
		problems = append(problems, fmt.Sprintf("request runs %d keyed probe scans above its budget of %d", probes, budget.Probes))
	}
	if limit := budget.Ceiling(limitCap); ceiling > limit {
		problems = append(problems, fmt.Sprintf("request proven worst case %.4g rows exceeds H+(P*K+B)*(C+1)=%.4g at the %d candidate cap", ceiling, limit, limitCap))
	}
	observed := float64(budget.Overhead) + (float64(budget.Probes)*(float64(budget.Allowance)+rounding)+float64(budget.Branches))*float64(candidates+1)
	if work > observed {
		problems = append(problems, fmt.Sprintf("request examined %.0f rows above H+(P*(K+r)+B)*(C+1)=%.0f for %d relevant candidates", work, observed, candidates))
	}
	return problems
}
