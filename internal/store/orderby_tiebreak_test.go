package store

// 本文件是 docs/lint-guards.md 的 L4 门禁：store 里每条 `ORDER BY` 的**末键必须唯一**。
//
// 为什么需要它（1461f7e4）：`ListUsersWithStatsPage` 曾用 `id`（`usr_` + crypto/rand）给
// 同秒用户破平局 —— 同毫秒创建的两个用户谁在前是掷硬币，`TestAdmin_ListUsers` 因此
// 42.5% 随机红（`-count=40` 失败 17 次）。修法是把末键改成 `rowid`。全仓 store 有 70 条
// `ORDER BY`，修完这一条之后没有任何机械约束防止下一条复发 —— 本文件就是那道约束。
//
// 判据（纯静态，不连数据库）：
//
//	① 抽出 internal/store 非测试源码里的 SQL：走 go/ast，覆盖反引号字符串、
//	   `+` 拼接、`query += ...` 累积、包级常量列清单（`capabilityColumns` 之类）；
//	② 解析 schema.sql：主键、`UNIQUE (...)`、`CREATE UNIQUE INDEX`（含部分索引的
//	   WHERE 谓词）、以及「非 WITHOUT ROWID 表才有 rowid」；
//	③ 逐条 `ORDER BY` 判定，只认四类**可核验**的安全证明（其余一律红）：
//	     · `末键唯一`       —— 末键是 rowid / 单列主键 / 单列唯一索引；
//	     · `分组键`         —— 结果集按 `GROUP BY` 分组，末键就是分组键（每组一行）；
//	     · `覆盖唯一键`     —— 末键自身不唯一，但「ORDER BY 全键 + WHERE 里 `=` 钉住的列」
//	                          合起来覆盖了某个唯一键，整条排序仍是全序（edges 的复合主键就是这种）；
//	     · `排序前缀覆盖`   —— 唯一键的列全部被 WHERE 等值钉住、末键只是它最前面的列时，
//	                          末键在**该结果集内**唯一（例：`WHERE task_id = ? ORDER BY attempt`）。
//	   判不动的两条出口：`orderByExemptions`（证明无害的白名单，必须写理由 + 可核验证据，
//	   过期即红）与 `orderByKnownGaps`（已确认的缺口，有上限预算，修好必须删登记）。
//	④ 白名单/缺口表**过期即红**（照 internal/server/openapi_parity_test.go 的例外表纪律）：
//	   登记项不再命中任何 ORDER BY 就失败，要求删除 —— 例外不许变成永久的后门。
//	⑤ 反假绿：解析到的条数必须 ≥ `orderByMinimumClauses`（实测值，出处见常量注释），
//	   解析到 0 条必须失败，并把条数/各类计数/白名单命中数打印出来。
//	⑥ 失败信息指名道姓：`文件:行 + 表名 + 子句原文 + 末键为什么不唯一 + 修法建议`。
//	⑦ 变异自证：把一条已知安全子句的末键替换成非唯一列（内存里改，不动源码文件），
//	   断言判据会红；再断言未变异的原句是绿的（红/绿双侧）。
//
// 纪律：本门禁只读源码与 schema.sql，**不改任何生产文件**；发现的产品缺口登记在
// `orderByKnownGaps` 并由协调者裁决，不在本文件里悄悄修掉。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ============================================================================
// 一、源码侧：把 store 里的 SQL 拼出来，抽出每一条 ORDER BY
// ============================================================================

// orderByChunk 是一段可解析的 SQL 文本片段，带它在源码里的起点（用于报 文件:行）。
type orderByChunk struct {
	file string
	line int
	col  int
	text string
}

// orderByText 是一条（可能由多段拼出来的）SQL 文本；unknown 记录拼不出来的子表达式
// （例如 `strings.Join(where, " AND ")`），只用于诊断，不影响已拼出部分的行号定位。
type orderByText struct {
	chunks  []orderByChunk
	unknown []string
}

func (t orderByText) String() string {
	var b strings.Builder
	for _, c := range t.chunks {
		b.WriteString(c.text)
	}
	return b.String()
}

// orderByOccurrence 是「某条 SQL 文本里第 N 个 ORDER BY 关键字」的定位结果。
type orderByOccurrence struct {
	file   string
	line   int
	col    int
	offset int    // 在整条 SQL 文本里的绝对偏移（指向 ORDER BY 的 O）
	clause string // 「ORDER BY ...」到子句结束的原文
}

// orderByFile 是一个待扫描的 Go 源文件（非测试）。
// ============================================================================
// 文件扫描：只读 internal/store 下的非测试 .go
// ============================================================================

// orderBySourceFiles 列出 internal/store 下的非测试 Go 源文件（相对文件名，已排序）。
//
// 为什么不用 filepath.Walk：本门禁只覆盖 store 这一层（schema.sql 与 SQL 都在这里），
// 递归下去会把 internal/server 的查询混进来，而那份的 schema 归属是另一套纪律。
func orderBySourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("列 internal/store 目录: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("internal/store 下没有扫到任何非测试 .go 源文件 —— 扫描器自己坏了")
	}
	return out
}

// orderByParsePackage 解析 store 包的所有非测试文件，返回文件集合与常量表。
func orderByParsePackage(t *testing.T, fset *token.FileSet) (map[string]*ast.File, map[string]orderByText) {
	t.Helper()
	files := make(map[string]*ast.File, 48)
	for _, name := range orderBySourceFiles(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		files[name] = f
	}
	return files, orderByPackageConsts(fset, files)
}

// orderByPackageConsts 收集包级字符串常量（含 `const a = b + "..."` 这种引用）。
//
// 为什么要它：`deploymentSelect` / `capabilityColumns` / `taskColumns` 这些常量是 SQL 的
// 一部分（FROM 子句就在里面），不解出来就判不出表名，等于把判据让给了「解析失败」。
func orderByPackageConsts(fset *token.FileSet, files map[string]*ast.File) map[string]orderByText {
	consts := make(map[string]orderByText, 64)
	// 三轮：允许常量之间跨文件前向引用（`deploymentSelect` 引用 `deploymentDigestSQL`）。
	for pass := 0; pass < 3; pass++ {
		for name, f := range files {
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != len(vs.Values) {
						continue
					}
					for i, ident := range vs.Names {
						if _, done := consts[ident.Name]; done {
							continue
						}
						if txt, ok := orderByResolveExpr(fset, name, consts, nil, vs.Values[i]); ok {
							consts[ident.Name] = txt
						}
					}
				}
			}
		}
	}
	return consts
}

// orderByResolveExpr 把一个 Go 字符串表达式解析成 SQL 文本片段序列。
//
// 认得：反引号/双引号字面量、`+` 拼接、括号、局部变量、包级常量。
// 不认得：函数调用等（例如 `strings.Join(where, " AND ")`、`placeholdersFor(n)`）——
// 这些返回 ok=false，由调用方决定是「忽略整条」还是「记进 unknown 继续拼」。
func orderByResolveExpr(fset *token.FileSet, file string, consts, locals map[string]orderByText, expr ast.Expr) (orderByText, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return orderByText{}, false
		}
		raw, err := strconv.Unquote(e.Value)
		if err != nil {
			return orderByText{}, false
		}
		pos := fset.Position(e.Pos())
		return orderByText{chunks: []orderByChunk{{file: file, line: pos.Line, col: pos.Column, text: raw}}}, true
	case *ast.ParenExpr:
		return orderByResolveExpr(fset, file, consts, locals, e.X)
	case *ast.Ident:
		if locals != nil {
			if txt, ok := locals[e.Name]; ok {
				return txt, true
			}
		}
		if txt, ok := consts[e.Name]; ok {
			return txt, true
		}
		return orderByText{}, false
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return orderByText{}, false
		}
		left, okLeft := orderByResolveExpr(fset, file, consts, locals, e.X)
		right, okRight := orderByResolveExpr(fset, file, consts, locals, e.Y)
		if !okLeft && !okRight {
			return orderByText{}, false
		}
		out := orderByText{}
		if okLeft {
			out.chunks = append(out.chunks, left.chunks...)
			out.unknown = append(out.unknown, left.unknown...)
		} else {
			out.unknown = append(out.unknown, orderByExprSite(fset, file, e.X))
		}
		if okRight {
			out.chunks = append(out.chunks, right.chunks...)
			out.unknown = append(out.unknown, right.unknown...)
		} else {
			out.unknown = append(out.unknown, orderByExprSite(fset, file, e.Y))
		}
		return out, true
	}
	return orderByText{}, false
}

// orderByExprSite 把「拼不出来的子表达式」记成 file:line，便于诊断。
func orderByExprSite(fset *token.FileSet, file string, expr ast.Expr) string {
	pos := fset.Position(expr.Pos())
	return fmt.Sprintf("%s:%d", file, pos.Line)
}

// orderByStatementsFromSource 扫一个文件，返回所有「可能是 SQL」的文本（按语句位置去过重）。
//
// 做法：沿函数体按源码顺序走语句，维护局部变量 → SQL 文本的映射（`query := ...` /
// `query += ...`），把每个调用实参与 return 值当 SQL 候选。同一处 ORDER BY 只会被
// 记一次：候选按「ORDER BY 关键字所在 文件:行:列」归并，取**最长**的那份文本 ——
// 最长的那份就是运行时真正执行的整条 SQL（局部变量累积够了，FROM 才在里面）。
func orderByStatementsFromSource(fset *token.FileSet, file string, f *ast.File, consts map[string]orderByText) []orderByText {
	var out []orderByText
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		locals := make(map[string]orderByText)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				// `q := ...` / `q = ...` / `q += ...`：维护局部变量累积。
				for i, lhs := range node.Lhs {
					ident, ok := lhs.(*ast.Ident)
					if !ok || ident.Name == "_" {
						continue
					}
					var rhs ast.Expr
					switch {
					case len(node.Rhs) == len(node.Lhs):
						rhs = node.Rhs[i]
					case len(node.Rhs) == 1:
						rhs = node.Rhs[0]
					default:
						continue
					}
					txt, ok := orderByResolveExpr(fset, file, consts, locals, rhs)
					if !ok {
						continue
					}
					if node.Tok == token.ADD_ASSIGN {
						prev := locals[ident.Name]
						txt.chunks = append(append([]orderByChunk{}, prev.chunks...), txt.chunks...)
						txt.unknown = append(append([]string{}, prev.unknown...), txt.unknown...)
					}
					locals[ident.Name] = txt
				}
				// 赋值右侧自身也可能是一条完整 SQL（不经过变量的写法）。
				for _, rhs := range node.Rhs {
					if txt, ok := orderByResolveExpr(fset, file, consts, locals, rhs); ok {
						out = append(out, txt)
					}
				}
			case *ast.CallExpr:
				for _, arg := range node.Args {
					if txt, ok := orderByResolveExpr(fset, file, consts, locals, arg); ok {
						out = append(out, txt)
					}
				}
			case *ast.ReturnStmt:
				for _, res := range node.Results {
					if txt, ok := orderByResolveExpr(fset, file, consts, locals, res); ok {
						out = append(out, txt)
					}
				}
			}
			return true
		})
	}
	return out
}

// orderByFindOccurrences 在一条 SQL 文本里找出所有 ORDER BY 关键字并定位到源码行。
func orderByFindOccurrences(stmt orderByText) []orderByOccurrence {
	text := stmt.String()
	lower := strings.ToLower(text)
	var out []orderByOccurrence
	for from := 0; ; {
		idx := strings.Index(lower[from:], "order by")
		if idx < 0 {
			break
		}
		off := from + idx
		from = off + len("order by")
		// 边界检查：避免把 `reorder by` 之类的子串当关键字（保留字对齐即可）。
		if off > 0 && isOrderByWordByte(lower[off-1]) {
			continue
		}
		if from < len(lower) && isOrderByWordByte(lower[from]) {
			continue
		}
		chunk, inChunk, ok := orderByLocateChunk(stmt, off)
		if !ok {
			continue
		}
		line := chunk.line
		col := chunk.col
		prefix := chunk.text[:inChunk]
		if n := strings.Count(prefix, "\n"); n > 0 {
			line += n
			col = len(prefix) - strings.LastIndex(prefix, "\n")
		} else {
			col += inChunk
		}
		out = append(out, orderByOccurrence{
			file:   chunk.file,
			line:   line,
			col:    col,
			offset: off,
			clause: orderByClauseText(text, off),
		})
	}
	return out
}

// isOrderByWordByte 报告 b 是否是 SQL 标识符/数字的一部分（用于关键字边界判断）。
func isOrderByWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// orderByLocateChunk 把整条 SQL 文本里的绝对偏移映射回具体片段及其内部偏移。
func orderByLocateChunk(stmt orderByText, offset int) (orderByChunk, int, bool) {
	seen := 0
	for _, c := range stmt.chunks {
		if offset < seen+len(c.text) {
			return c, offset - seen, true
		}
		seen += len(c.text)
	}
	return orderByChunk{}, 0, false
}

// orderByClauseText 从 ORDER BY 关键字处抽出整条子句原文（到同层 `)` / `;` / 文本结尾）。
//
// 为什么要跟踪括号：清理型 SQL 是 `DELETE ... WHERE id IN (SELECT ... ORDER BY x LIMIT ?)`，
// 子句到同层 `)` 就结束了，多抓会把 IN 的右括号后面那截空文本也算进来。
func orderByClauseText(text string, orderByOffset int) string {
	depth := 0
	inQuote := false
	for i := orderByOffset; i < len(text); i++ {
		c := text[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return strings.TrimSpace(text[orderByOffset:i])
			}
			depth--
		case ';':
			if depth == 0 {
				return strings.TrimSpace(text[orderByOffset:i])
			}
		}
	}
	return strings.TrimSpace(text[orderByOffset:])
}

// ============================================================================
// 二、schema.sql 侧：主键 / UNIQUE 约束 / 唯一索引 / rowid 可用性
// ============================================================================

// orderByUniqueKey 是一条唯一性事实（主键、UNIQUE 约束或 CREATE UNIQUE INDEX）。
type orderByUniqueKey struct {
	columns  []string // 列组合（小写）
	partial  string   // 非空 = 部分唯一索引的 WHERE 谓词原文；只有查询真的带这个谓词才可依赖
	evidence string   // 可核验证据：`PRIMARY KEY (...)` / `UNIQUE (...)` / `CREATE UNIQUE INDEX name`
}

// orderByTable 是 schema 里一张表在本门禁眼里需要的全部事实。
type orderByTable struct {
	name     string
	hasRowid bool // 非 WITHOUT ROWID 表才有 rowid 可用
	uniques  []orderByUniqueKey
}

// orderBySchema 是 schema.sql 的静态事实。
type orderBySchema struct {
	tables map[string]*orderByTable
}

var (
	// orderByCreateTableRe / orderByCreateIndexRe 只做「定位括号起点」这一步，列与谓词的解析
	// 交给后面的括号/逗号扫描（正则切不开 `CHECK (id = 1)` 这种带逗号的列定义）。
	orderByCreateTableRe = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?("[^"]+"|[A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	orderByCreateIndexRe = regexp.MustCompile(`(?is)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?("[^"]+"|[A-Za-z_][A-Za-z0-9_]*)\s+ON\s+("[^"]+"|[A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	// orderByPartialPredicateRe 认「部分唯一索引」的谓词形态：`<col> IS [NOT] NULL`。
	// schema 里的部分索引只有这一种（idx_cap_live_name_ver / idx_cap_live_digest /
	// idx_emits_task_emit / idx_tasks_pending_created_at / idx_task_attempts_unreaped）。
	// 认不出的谓词一律按「无法证明」处理（fail closed），不猜。
	orderByPartialPredicateRe = regexp.MustCompile(`(?is)^\s*([A-Za-z_][A-Za-z0-9_]*)\s+IS\s+(NOT\s+)?NULL\s*$`)
	// orderByPinRe 认 WHERE 里「列 = 常量/参数」这种等值钉住。
	orderByPinRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(\?|'[^']*'|-?\d+)`)
	// orderByBarePinRe 认不带限定符的等值：`task_id = ?`。
	orderByBarePinRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(\?|'[^']*'|-?\d+)`)
	// orderByJoinOnRe 认 JOIN 的 ON 等值对，用于判定「这条 JOIN 会不会把行放大」。
	orderByJoinOnRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)`)
	// orderByBareIdentRe / orderByQualifiedIdentRe 是排序键的两种合法形态。
	orderByBareIdentRe      = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	orderByQualifiedIdentRe = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)$`)
)

// orderByLoadSchema 解析 internal/store/schema.sql。
//
// 只收本门禁需要的事实：每张表的唯一键（主键 / UNIQUE 约束 / 唯一索引，含部分索引的谓词）
// 与「有没有 rowid」。CHECK / 外键 / 默认值 / 类型都不进来 —— 判据用不到，收了只会腐坏。
func orderByLoadSchema(t *testing.T) *orderBySchema {
	t.Helper()
	raw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("读 schema.sql: %v", err)
	}
	sch := &orderBySchema{tables: make(map[string]*orderByTable)}
	for _, stmt := range orderBySplitStatements(orderByStripLineComments(string(raw))) {
		if m := orderByCreateTableRe.FindStringSubmatch(stmt); m != nil {
			sch.addTable(stmt, m)
			continue
		}
		if m := orderByCreateIndexRe.FindStringSubmatch(stmt); m != nil {
			sch.addIndex(stmt, m)
		}
	}
	if len(sch.tables) == 0 {
		t.Fatalf("schema.sql 里一张表都没解析出来 —— schema 解析器自己坏了（反假绿）")
	}
	return sch
}

// orderByStripLineComments 去掉 SQL 行注释（`-- ...`），保留换行以便行号不错乱。
// 单引号字符串里的 `--` 不算注释（schema 里有 `'[]'`、`'unixepoch'` 这类默认值）。
func orderByStripLineComments(src string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inQuote {
			b.WriteByte(c)
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		if c == '\'' {
			inQuote = true
			b.WriteByte(c)
			continue
		}
		if c == '-' && i+1 < len(src) && src[i+1] == '-' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// orderBySplitStatements 按顶层分号切语句（括号与引号内的分号不切）。
func orderBySplitStatements(src string) []string {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				if stmt := strings.TrimSpace(src[start:i]); stmt != "" {
					out = append(out, stmt)
				}
				start = i + 1
			}
		}
	}
	if stmt := strings.TrimSpace(src[start:]); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

// orderByMatchingParen 返回 open 处 `(` 对应的 `)` 下标（找不到返回 -1）。
func orderByMatchingParen(s string, open int) int {
	depth := 0
	inQuote := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// orderBySplitTopLevel 按顶层逗号切（括号与引号内的逗号不切）。
func orderBySplitTopLevel(s string) []string {
	var out []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				if item := strings.TrimSpace(s[start:i]); item != "" {
					out = append(out, item)
				}
				start = i + 1
			}
		}
	}
	if item := strings.TrimSpace(s[start:]); item != "" {
		out = append(out, item)
	}
	return out
}

// orderByTopLevelTokens 把一段列定义按「顶层空白」切成 token（括号/引号内的空白不切）。
// 例：`id INTEGER PRIMARY KEY CHECK (id = 1)` → [id, INTEGER, PRIMARY, KEY, CHECK (id = 1)]。
func orderByTopLevelTokens(s string) []string {
	var out []string
	depth := 0
	inQuote := false
	var cur strings.Builder
	flush := func() {
		if tok := strings.TrimSpace(cur.String()); tok != "" {
			out = append(out, tok)
		}
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			cur.WriteByte(c)
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
			cur.WriteByte(c)
		case '(':
			depth++
			cur.WriteByte(c)
		case ')':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(c)
		case ' ', '\t', '\n', '\r':
			if depth > 0 {
				cur.WriteByte(c)
			} else {
				flush()
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// orderByIdent 去掉标识符两侧的引号（`"values"` / `[x]` / 反引号）并转小写。
func orderByIdent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`[]")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.Trim(s, `"`)
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// orderByColumnsFromParenGroup 取出 `(...)` 里的列名列表。
func orderByColumnsFromParenGroup(s string) []string {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return nil
	}
	closeIdx := orderByMatchingParen(s, open)
	if closeIdx < 0 {
		return nil
	}
	var cols []string
	for _, item := range orderBySplitTopLevel(s[open+1 : closeIdx]) {
		// 列项可能带 COLLATE / ASC / DESC（索引定义里合法），只取第一个 token 当列名。
		toks := orderByTopLevelTokens(item)
		if len(toks) == 0 {
			continue
		}
		cols = append(cols, orderByIdent(toks[0]))
	}
	return cols
}

// addTable 解析一条 CREATE TABLE：列级/表级 PRIMARY KEY 与 UNIQUE、WITHOUT ROWID。
func (s *orderBySchema) addTable(stmt string, m []string) {
	name := orderByIdent(m[1])
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return
	}
	closeIdx := orderByMatchingParen(stmt, open)
	if closeIdx < 0 {
		return
	}
	tbl := &orderByTable{
		name:     name,
		hasRowid: !strings.Contains(strings.ToUpper(stmt[closeIdx:]), "WITHOUT ROWID"),
	}
	for _, item := range orderBySplitTopLevel(stmt[open+1 : closeIdx]) {
		toks := orderByTopLevelTokens(item)
		if len(toks) == 0 {
			continue
		}
		// flat 只看「这条定义是什么」，列名与括号内容仍从原始 item 取。
		flat := strings.ToUpper(orderByFlatten(item))
		if strings.HasPrefix(flat, "CONSTRAINT ") {
			// `CONSTRAINT name CHECK (...)` / `CONSTRAINT name PRIMARY KEY (...)`：
			// 去掉 `CONSTRAINT` 与约束名两个 token，再看真实约束关键字。
			parts := strings.SplitN(flat, " ", 4)
			if len(parts) < 4 {
				continue
			}
			flat = parts[3]
		}
		switch {
		case strings.HasPrefix(flat, "PRIMARY KEY"):
			cols := orderByColumnsFromParenGroup(item)
			if len(cols) > 0 {
				tbl.uniques = append(tbl.uniques, orderByUniqueKey{
					columns:  cols,
					evidence: "PRIMARY KEY (" + strings.Join(cols, ", ") + ")",
				})
			}
		case strings.HasPrefix(flat, "UNIQUE"):
			cols := orderByColumnsFromParenGroup(item)
			if len(cols) > 0 {
				tbl.uniques = append(tbl.uniques, orderByUniqueKey{
					columns:  cols,
					evidence: "UNIQUE (" + strings.Join(cols, ", ") + ")",
				})
			}
		case strings.HasPrefix(flat, "FOREIGN") || strings.HasPrefix(flat, "CHECK"):
			// 与本门禁无关（外键/CHECK 不产生唯一性事实）。
		default:
			// 列定义：列名 + 类型/约束 token。只认顶层出现的 PRIMARY KEY / UNIQUE。
			col := orderByIdent(toks[0])
			for i := 1; i < len(toks); i++ {
				up := strings.ToUpper(toks[i])
				if up == "PRIMARY" && i+1 < len(toks) && strings.ToUpper(toks[i+1]) == "KEY" {
					tbl.uniques = append(tbl.uniques, orderByUniqueKey{
						columns:  []string{col},
						evidence: "列级 PRIMARY KEY（" + col + "）",
					})
					i++
					continue
				}
				if up == "UNIQUE" {
					tbl.uniques = append(tbl.uniques, orderByUniqueKey{
						columns:  []string{col},
						evidence: "列级 UNIQUE（" + col + "）",
					})
				}
			}
		}
	}
	s.tables[name] = tbl
}

// addIndex 解析一条 CREATE UNIQUE INDEX（含部分索引的 WHERE 谓词）。
//
// 非唯一索引直接忽略：它们不提供「列组合唯一」这条事实（`idx_cap_name_ver` 就是非唯一的）。
func (s *orderBySchema) addIndex(stmt string, m []string) {
	if strings.TrimSpace(m[1]) == "" {
		return
	}
	indexName := orderByIdent(m[2])
	table := orderByIdent(m[3])
	tbl, ok := s.tables[table]
	if !ok {
		return
	}
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return
	}
	closeIdx := orderByMatchingParen(stmt, open)
	if closeIdx < 0 {
		return
	}
	cols := orderByColumnsFromParenGroup(stmt[open:])
	if len(cols) == 0 {
		return
	}
	partial := ""
	if tail := strings.TrimSpace(stmt[closeIdx+1:]); tail != "" {
		if idx := strings.Index(strings.ToLower(tail), "where"); idx >= 0 {
			partial = orderByFlatten(tail[idx+len("where"):])
		}
	}
	tbl.uniques = append(tbl.uniques, orderByUniqueKey{
		columns:  cols,
		partial:  partial,
		evidence: "CREATE UNIQUE INDEX " + indexName + " (" + strings.Join(cols, ", ") + ")",
	})
}

// ============================================================================
// 三、逐条 ORDER BY 的静态分析（表 / WHERE 钉住的列 / JOIN / GROUP BY）
// ============================================================================

// orderByJoin 是一条 JOIN 及其 ON 等值对被 JOIN 表一侧的列。
type orderByJoin struct {
	table     string
	alias     string
	onColumns []string // 被 JOIN 表在 ON 里参与等值的列（用于判「不会放大行数」）
}

// orderByClause 是一条 ORDER BY 的全部静态事实。
type orderByClause struct {
	file        string
	line        int
	sql         string // 整条 SQL 原文（判据与变异都基于它）
	orderOffset int    // ORDER BY 关键字在 sql 里的偏移（变异后重新定位用）
	// clause 是子句原文（空白压平），clauseKey 是小写版（白名单/缺口表按它匹配）。
	clause    string
	clauseKey string
	// 规范化排序键（小写，去掉 ASC/DESC/COLLATE）。
	keys []string
	// 驱动表与限定符 → 表（别名/表名都能当限定符）。
	table      string
	qualifiers map[string]string
	// WHERE 里被 `=` 钉死的列：表 → 列集合。
	pinned map[string]map[string]bool
	// WHERE 段（只保留 AND 连通的片段，OR 分支整段丢弃 —— fail closed）。
	where string
	// GROUP BY 键（规范化列名）。
	groupKeys []string
	joins     []orderByJoin
	// 解析阶段的问题（表名解析不出、语句里找不到 FROM 等）。
	problems []string
	// SQL 里拼不出来（函数调用等）的子表达式位置，失败信息里要能说明「可能因此判不出表」。
	unresolved []string
}

// orderByScanClauses 扫全包，返回按 文件:行 排序、按 ORDER BY 出现位置去重后的子句表。
func orderByScanClauses(t *testing.T) []orderByClause {
	t.Helper()
	fset := token.NewFileSet()
	files, consts := orderByParsePackage(t, fset)

	type occurrence struct {
		stmt orderByText // 该出现位置所属的那条 SQL（取最长的一份）
		occ  orderByOccurrence
	}
	best := make(map[string]occurrence)
	for name, f := range files {
		for _, stmt := range orderByStatementsFromSource(fset, name, f, consts) {
			for _, occ := range orderByFindOccurrences(stmt) {
				key := fmt.Sprintf("%s:%d:%d", occ.file, occ.line, occ.col)
				cur, ok := best[key]
				if !ok || len(stmt.String()) > len(cur.stmt.String()) {
					best[key] = occurrence{stmt: stmt, occ: occ}
				}
			}
		}
	}
	if len(best) == 0 {
		t.Fatalf("没有扫到任何 ORDER BY —— 扫描器自己坏了（反假绿：0 条必须红）")
	}

	var out []orderByClause
	for _, oc := range best {
		out = append(out, orderByAnalyzeClause(oc.stmt, oc.occ))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

// orderByAnalyzeClause 从「整条 SQL + ORDER BY 位置」推出这条子句的全部事实。
func orderByAnalyzeClause(stmt orderByText, occ orderByOccurrence) orderByClause {
	text := stmt.String()
	cl := orderByClause{
		file:        occ.file,
		line:        occ.line,
		sql:         text,
		orderOffset: occ.offset,
		unresolved:  stmt.unknown,
		clause:      orderByFlatten(occ.clause),
		clauseKey:   orderByNormalizeClause(occ.clause),
		qualifiers:  make(map[string]string),
		pinned:      make(map[string]map[string]bool),
	}
	cl.keys = orderByParseSortKeys(occ.clause)
	if len(cl.keys) == 0 {
		cl.problems = append(cl.problems, "ORDER BY 后面解析不出任何排序键")
	}

	depth := orderByDepthAt(text, occ.offset)
	fromIdx := orderByLastKeyword(text, "from", occ.offset, depth)
	if fromIdx < 0 {
		cl.problems = append(cl.problems, "ORDER BY 之前找不到同层 FROM：表名无法确定")
		return cl
	}
	table, alias, afterAlias := orderByParseTableRef(text, fromIdx+len("from"))
	if table == "" {
		cl.problems = append(cl.problems, "FROM 后面解析不出表名")
		return cl
	}
	cl.table = table
	cl.qualifiers[table] = table
	if alias != "" {
		cl.qualifiers[alias] = table
	}
	cl.joins = orderByParseJoins(text, afterAlias, occ.offset, depth, cl.qualifiers)

	if whereIdx := orderByLastKeyword(text, "where", occ.offset, depth); whereIdx >= 0 {
		cl.where = orderByAndSpans(text[whereIdx+len("where") : occ.offset])
		orderByCollectPins(&cl)
	}
	if groupIdx := orderByLastKeyword(text, "groupby", occ.offset, depth); groupIdx >= 0 {
		groupText := text[groupIdx+len("group by") : occ.offset]
		if cut := orderByCutAtClauseKeyword(groupText); cut != "" {
			groupText = cut
		}
		for _, key := range orderBySplitTopLevel(groupText) {
			if col, _, ok := orderByNormalizeSortKey(key); ok {
				cl.groupKeys = append(cl.groupKeys, col)
			}
		}
	}
	return cl
}

// orderByFlatten 折叠空白（保留大小写）。
func orderByFlatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// orderByNormalizeClause 小写 + 折叠空白：白名单/缺口表与扫描结果都用这一份规范形态。
func orderByNormalizeClause(s string) string {
	return strings.ToLower(orderByFlatten(s))
}

// orderByParseSortKeys 把 ORDER BY 子句切成排序键（到同层 LIMIT/OFFSET 为止）。
func orderByParseSortKeys(clause string) []string {
	body := clause
	if lower := strings.ToLower(strings.TrimSpace(body)); strings.HasPrefix(lower, "order by") {
		body = strings.TrimSpace(body)[len("order by"):]
	}
	if cut := orderByCutAtClauseKeyword(body); cut != "" {
		body = cut
	}
	var keys []string
	for _, item := range orderBySplitTopLevel(body) {
		if item = strings.TrimSpace(item); item != "" {
			keys = append(keys, item)
		}
	}
	return keys
}

// orderByCutAtClauseKeyword 截到同层 LIMIT / OFFSET 之前（排序键列表到此结束）。
func orderByCutAtClauseKeyword(s string) string {
	lower := strings.ToLower(s)
	depth := 0
	inQuote := false
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && (i == 0 || !isOrderByWordByte(lower[i-1])) {
				if strings.HasPrefix(lower[i:], "limit") && (i+5 == len(lower) || !isOrderByWordByte(lower[i+5])) {
					return s[:i]
				}
				if strings.HasPrefix(lower[i:], "offset") && (i+6 == len(lower) || !isOrderByWordByte(lower[i+6])) {
					return s[:i]
				}
			}
		}
	}
	return ""
}

// orderByNormalizeSortKey 把一个排序键规范化成 (列名, 限定符, 是否列)。
//
// 认 `col`、`t.col`、`col ASC|DESC`、`col COLLATE x`；其余（函数调用、CASE 表达式）返回 ok=false。
func orderByNormalizeSortKey(key string) (column, qualifier string, ok bool) {
	k := orderByFlatten(strings.ToLower(key))
	for {
		switch {
		case strings.HasSuffix(k, " asc"):
			k = strings.TrimSuffix(k, " asc")
			continue
		case strings.HasSuffix(k, " desc"):
			k = strings.TrimSuffix(k, " desc")
			continue
		}
		if idx := strings.Index(k, " collate "); idx >= 0 {
			k = strings.TrimSpace(k[:idx])
			continue
		}
		break
	}
	if m := orderByQualifiedIdentRe.FindStringSubmatch(k); m != nil {
		return m[2], m[1], true
	}
	if orderByBareIdentRe.MatchString(k) {
		return k, "", true
	}
	return "", "", false
}

// orderByDepthAt 返回 offset 处的括号深度（忽略单引号字符串里的括号）。
func orderByDepthAt(s string, offset int) int {
	depth := 0
	inQuote := false
	for i := 0; i < offset && i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

// orderByLastKeyword 返回 before 之前、深度为 depth 的最后一个关键字位置（找不到 -1）。
// keyword 为 `groupby` 时匹配 `GROUP BY`（两个词，中间任意空白）。
func orderByLastKeyword(s, keyword string, before, depth int) int {
	lower := strings.ToLower(s)
	found := -1
	curDepth := 0
	inQuote := false
	for i := 0; i < before && i < len(lower); i++ {
		c := lower[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
			continue
		case '(':
			curDepth++
			continue
		case ')':
			if curDepth > 0 {
				curDepth--
			}
			continue
		}
		if curDepth != depth || (i > 0 && isOrderByWordByte(lower[i-1])) {
			continue
		}
		if keyword == "groupby" {
			rest := lower[i:]
			if strings.HasPrefix(rest, "group") && (len(rest) == 5 || !isOrderByWordByte(rest[5])) {
				after := strings.TrimLeft(rest[5:], " \t\r\n")
				if strings.HasPrefix(after, "by") && (len(after) == 2 || !isOrderByWordByte(after[2])) {
					found = i
				}
			}
			continue
		}
		if strings.HasPrefix(lower[i:], keyword) && (i+len(keyword) == len(lower) || !isOrderByWordByte(lower[i+len(keyword)])) {
			found = i
		}
	}
	return found
}

// orderByParseTableRef 从 FROM/JOIN 之后解析「表名 [别名]」，返回表、别名与别名之后的偏移。
func orderByParseTableRef(s string, pos int) (table, alias string, next int) {
	pos = orderBySkipSpace(s, pos)
	start := pos
	for pos < len(s) && (isOrderByWordByte(s[pos]) || s[pos] == '.') {
		pos++
	}
	table = orderByIdent(s[start:pos])
	if table == "" {
		return "", "", pos
	}
	afterTable := pos
	pos = orderBySkipSpace(s, pos)
	wordStart := pos
	for pos < len(s) && isOrderByWordByte(s[pos]) {
		pos++
	}
	candidate := strings.ToLower(s[wordStart:pos])
	if candidate != "" && !orderByReservedWord(candidate) {
		return table, candidate, pos
	}
	return table, "", afterTable
}

// orderBySkipSpace 跳过空白。
func orderBySkipSpace(s string, pos int) int {
	for pos < len(s) {
		switch s[pos] {
		case ' ', '\t', '\n', '\r':
			pos++
		default:
			return pos
		}
	}
	return pos
}

// orderByReservedWord 报告某个词是否是「不可能是表别名」的 SQL 关键字。
func orderByReservedWord(w string) bool {
	switch w {
	case "where", "join", "left", "right", "inner", "outer", "cross", "natural", "on",
		"using", "group", "by", "order", "limit", "offset", "union", "having", "as",
		"and", "or", "not", "when", "then", "else", "end", "select", "from", "set":
		return true
	}
	return false
}

// orderByParseJoins 收集 [start, end) 里同层的 JOIN 及其 ON 等值列。
func orderByParseJoins(s string, start, end, depth int, qualifiers map[string]string) []orderByJoin {
	var out []orderByJoin
	for pos := start; pos < end; {
		idx := orderByNextKeyword(s, "join", pos, end, depth)
		if idx < 0 {
			break
		}
		table, alias, afterAlias := orderByParseTableRef(s, idx+len("join"))
		join := orderByJoin{table: table, alias: alias}
		onIdx := orderByNextKeyword(s, "on", afterAlias, end, depth)
		if onIdx < 0 {
			if table != "" {
				qualifiers[table] = table
			}
			if alias != "" {
				qualifiers[alias] = table
			}
			out = append(out, join)
			pos = afterAlias
			continue
		}
		onEnd := end
		for _, kw := range []string{"join", "where", "group", "order", "limit"} {
			if next := orderByNextKeyword(s, kw, onIdx+len("on"), end, depth); next >= 0 && next < onEnd {
				onEnd = next
			}
		}
		segment := s[onIdx+len("on") : onEnd]
		for _, m := range orderByJoinOnRe.FindAllStringSubmatch(segment, -1) {
			leftQual, leftCol := strings.ToLower(m[1]), strings.ToLower(m[2])
			rightQual, rightCol := strings.ToLower(m[3]), strings.ToLower(m[4])
			switch {
			case orderByLeavesJoinSide(leftQual, table, alias):
				join.onColumns = append(join.onColumns, leftCol)
			case orderByLeavesJoinSide(rightQual, table, alias):
				join.onColumns = append(join.onColumns, rightCol)
			}
		}
		if table != "" {
			qualifiers[table] = table
		}
		if alias != "" {
			qualifiers[alias] = table
		}
		out = append(out, join)
		pos = onEnd
	}
	return out
}

// orderByLeavesJoinSide 报告某个限定符是否指向被 JOIN 的那张表（决定 ON 里哪一侧的列算它的）。
func orderByLeavesJoinSide(qualifier, table, alias string) bool {
	return qualifier != "" && (qualifier == alias || qualifier == table)
}

// orderByNextKeyword 返回 pos 之后第一个深度为 depth 的关键字位置。
func orderByNextKeyword(s, keyword string, pos, end, depth int) int {
	lower := strings.ToLower(s)
	curDepth := orderByDepthAt(s, pos)
	inQuote := false
	for i := pos; i < end && i < len(lower); i++ {
		c := lower[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
			continue
		case '(':
			curDepth++
			continue
		case ')':
			if curDepth > 0 {
				curDepth--
			}
			continue
		}
		if curDepth != depth || (i > 0 && isOrderByWordByte(lower[i-1])) {
			continue
		}
		if strings.HasPrefix(lower[i:], keyword) && (i+len(keyword) == len(lower) || !isOrderByWordByte(lower[i+len(keyword)])) {
			return i
		}
	}
	return -1
}

// orderByAndSpans 只保留 AND 连通的片段：遇到含顶层 OR 的括号组整段丢弃。
//
// 为什么必须这样做（fail closed）：`WHERE (name = ? OR version < ?)` 里 `name = ?`
// 只是 OR 的一个分支，不能当「name 被钉死」用。宁可直接放弃这组线索，也不许
// 把分支条件当成等值钉住 —— 那是把判据放宽成「看起来有 WHERE 就算安全」。
func orderByAndSpans(where string) string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	groupStart := -1
	for i := 0; i < len(where); i++ {
		c := where[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			if depth == 0 {
				groupStart = i
				if seg := strings.TrimSpace(where[start:i]); seg != "" {
					parts = append(parts, seg)
				}
			}
			depth++
		case ')':
			if depth > 0 {
				depth--
				if depth == 0 && groupStart >= 0 {
					inner := where[groupStart+1 : i]
					if !orderByHasTopLevelOr(inner) {
						parts = append(parts, orderByAndSpans(inner))
					}
					start = i + 1
					groupStart = -1
				}
			}
		}
	}
	if depth == 0 {
		if seg := strings.TrimSpace(where[start:]); seg != "" {
			parts = append(parts, seg)
		}
	}
	return strings.ToLower(orderByFlatten(strings.Join(parts, " ")))
}

// orderByHasTopLevelOr 报告片段里是否有顶层 `OR`。
func orderByHasTopLevelOr(s string) bool {
	lower := strings.ToLower(s)
	depth := 0
	inQuote := false
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 && (i == 0 || !isOrderByWordByte(lower[i-1])) &&
				strings.HasPrefix(lower[i:], "or") && (i+2 == len(lower) || !isOrderByWordByte(lower[i+2])) {
				return true
			}
		}
	}
	return false
}

// orderByCollectPins 收集 WHERE（AND 连通部分）里被 `=` 钉死的列。
//
// 带限定符的列按 `限定符 → 表` 归属；不带限定符的列只在**单表查询**里归给驱动表 ——
// 多表时裸列到底属于谁光看文本判定不了，按「证不出就不算」处理（fail closed）。
func orderByCollectPins(cl *orderByClause) {
	add := func(table, col string) {
		if table == "" || col == "" {
			return
		}
		if cl.pinned[table] == nil {
			cl.pinned[table] = make(map[string]bool)
		}
		cl.pinned[table][col] = true
	}
	for _, m := range orderByPinRe.FindAllStringSubmatch(cl.where, -1) {
		if table, ok := cl.qualifiers[strings.ToLower(m[1])]; ok {
			add(table, strings.ToLower(m[2]))
		}
	}
	if len(cl.joins) > 0 {
		return
	}
	for _, m := range orderByBarePinRe.FindAllStringSubmatch(cl.where, -1) {
		col := strings.ToLower(m[1])
		if col == "rowid" {
			continue
		}
		add(cl.table, col)
	}
}

// ============================================================================
// 四、判定：一条 ORDER BY 凭什么算秩定
// ============================================================================

// orderByProof 是一条 ORDER BY 被视为「秩定」的证据（class 为空 = 没证出来）。
type orderByProof struct {
	class    string
	evidence string
}

// orderByJudge 判定一条 ORDER BY 是否秩定 —— 纯函数（变异自证拿它直接进内存判据）。
//
// 三类证明（都要求可核验的事实，不猜）：
//   - 分组键：结果集按 GROUP BY 分好组了，末键就是分组键 → 每组一行，天然唯一；
//   - 末键唯一：末键是 rowid（表不是 WITHOUT ROWID）或单列唯一键（含部分唯一索引的谓词被满足）；
//   - 覆盖唯一键：末键自身不唯一，但「ORDER BY 全键 ∪ WHERE 里 `=` 钉住的列」覆盖了某个
//     唯一键 —— 两行在上面这些列上全等就意味着它们是同一行，整条排序仍是全序。
//     JOIN 必须是 1:1（ON 的列在被 JOIN 表上是唯一键），否则一行会被放大成多行。
func orderByJudge(cl orderByClause, sch *orderBySchema) (orderByProof, []string) {
	proof, problems := orderByJudgeCore(cl, sch)
	if proof.class != "" && len(problems) > 0 {
		// 有证明、但同时有「推翻它」的问题（典型：JOIN 不 1:1，一行会被放大成多行）——
		// 证明作废。这是 fail closed 的关键一步：不许「一边报问题一边算通过」。
		return orderByProof{}, problems
	}
	return proof, problems
}

// orderByJudgeCore 是判据的实体（见 orderByJudge 的契约）。
func orderByJudgeCore(cl orderByClause, sch *orderBySchema) (orderByProof, []string) {
	if len(cl.keys) == 0 {
		return orderByProof{}, []string{"ORDER BY 后面解析不出任何排序键"}
	}
	lastRaw := cl.keys[len(cl.keys)-1]
	lastCol, lastQual, isColumn := orderByNormalizeSortKey(lastRaw)
	if !isColumn {
		return orderByProof{}, []string{fmt.Sprintf(
			"末键 %q 是表达式而不是列：表达式上没有唯一性事实可依赖（把表达式放前面、末键留一列）", lastRaw)}
	}

	// ① 分组键：结果集每组一行。
	for _, gk := range cl.groupKeys {
		if gk == lastCol {
			return orderByProof{
				class:    "分组键",
				evidence: fmt.Sprintf("结果集按 GROUP BY 分组，末键 %s 就是分组键（每组一行）", gk),
			}, nil
		}
	}

	// ② 定位末键所属的表。
	table := cl.table
	if lastQual != "" {
		resolved, ok := cl.qualifiers[lastQual]
		if !ok {
			return orderByProof{}, []string{fmt.Sprintf(
				"末键 %q 的限定符 %q 解析不到表（既不是驱动表别名，也不是 JOIN 进来的表）", lastRaw, lastQual)}
		}
		table = resolved
	}
	if table == "" {
		return orderByProof{}, []string{"没有解析出驱动表，无法核对唯一性事实"}
	}
	tbl := sch.tables[table]
	if tbl == nil {
		return orderByProof{}, []string{fmt.Sprintf(
			"schema.sql 里没有表 %q —— 这张表不在 schema 管辖内（SQLite 目录表 / pragma 虚表？）", table)}
	}

	// ③ JOIN 必须是 1:1，否则唯一键不蕴含「结果集里每行只出现一次」。
	var joinProblems []string
	for _, j := range cl.joins {
		jt := sch.tables[j.table]
		if jt == nil {
			joinProblems = append(joinProblems, fmt.Sprintf("JOIN 进来的表 %q 不在 schema.sql 里", j.table))
			continue
		}
		if len(j.onColumns) == 0 {
			joinProblems = append(joinProblems, fmt.Sprintf(
				"JOIN %s 的 ON 条件解析不出 `x.col = y.col` 等值对，无法证明它不会放大行数", j.table))
			continue
		}
		if !orderByJoinIsOneToOne(jt, j.onColumns, cl) {
			joinProblems = append(joinProblems, fmt.Sprintf(
				"JOIN %s 的 ON 列 %v 在 %s 上不构成唯一键：这条 JOIN 可能把一行放大成多行",
				j.table, j.onColumns, j.table))
		}
	}

	// ④ 末键唯一：rowid 或单列唯一键。
	if lastCol == "rowid" {
		if tbl.hasRowid {
			return orderByProof{
				class:    "末键唯一",
				evidence: fmt.Sprintf("末键 rowid：%s 不是 WITHOUT ROWID 表，rowid 天然唯一", table),
			}, joinProblems
		}
		return orderByProof{}, []string{fmt.Sprintf("末键是 rowid，但 %s 是 WITHOUT ROWID 表（没有 rowid 可用）", table)}
	}
	for _, u := range tbl.uniques {
		if len(u.columns) != 1 || u.columns[0] != lastCol {
			continue
		}
		distinct, why := orderByPartialDistinct(u, cl, table)
		if !distinct {
			return orderByProof{}, append(joinProblems, why)
		}
		proof := orderByProof{
			class:    "末键唯一",
			evidence: fmt.Sprintf("末键 %s 被 %s 覆盖", lastCol, u.evidence),
		}
		if u.partial != "" {
			proof.evidence += fmt.Sprintf("（部分唯一索引，谓词 %q 在 WHERE 里已满足）", u.partial)
		}
		return proof, joinProblems
	}

	// ⑤ 覆盖唯一键：ORDER BY 全键 ∪ WHERE 等值钉住的列 ⊇ 某个唯一键。
	covered := map[string]bool{}
	for _, key := range cl.keys {
		if col, _, ok := orderByNormalizeSortKey(key); ok {
			covered[col] = true
		}
	}
	pinnedCols := make([]string, 0, len(cl.pinned[table]))
	for col := range cl.pinned[table] {
		covered[col] = true
		pinnedCols = append(pinnedCols, col)
	}
	sort.Strings(pinnedCols)
	for _, u := range tbl.uniques {
		if !orderByColumnsAllIn(u.columns, covered) {
			continue
		}
		distinct, why := orderByPartialDistinct(u, cl, table)
		if !distinct {
			return orderByProof{}, append(joinProblems, why)
		}
		missing := orderByColumnsNotInKeys(u.columns, cl.keys)
		evidence := fmt.Sprintf("末键 %s 不单独唯一，但 ORDER BY 全键覆盖了 %s；其中 %v 由 WHERE 等值钉住",
			lastCol, u.evidence, missing)
		if u.partial != "" {
			evidence += fmt.Sprintf("（部分唯一索引，谓词 %q 在 WHERE 里已满足）", u.partial)
		}
		return orderByProof{class: "覆盖唯一键", evidence: evidence}, joinProblems
	}

	// ⑥ 证不出来：把「末键为什么不唯一」写清楚。
	problems := append([]string{}, joinProblems...)
	if len(pinnedCols) > 0 {
		problems = append(problems, fmt.Sprintf(
			"WHERE 里被 `=` 钉住的 %s 列 %v 不够覆盖任何一个唯一键", table, pinnedCols))
	}
	return orderByProof{}, append(problems, fmt.Sprintf("末键 %s 在 %s 上不唯一", lastCol, table))
}

// orderByJoinIsOneToOne 报告 ON 的列是否构成被 JOIN 表的一个唯一键（= JOIN 至多 1:1）。
func orderByJoinIsOneToOne(tbl *orderByTable, onColumns []string, cl orderByClause) bool {
	onSet := map[string]bool{}
	for _, c := range onColumns {
		onSet[c] = true
	}
	for _, u := range tbl.uniques {
		if !orderByColumnsAllIn(u.columns, onSet) {
			continue
		}
		if distinct, _ := orderByPartialDistinct(u, cl, tbl.name); distinct {
			return true
		}
	}
	return false
}

// orderByPartialDistinct 判断一条唯一键在**这条查询**里是否真的唯一：
// 非部分索引直接可用；部分唯一索引要求它的 WHERE 谓词确实出现在本条 SQL 的 WHERE 里。
func orderByPartialDistinct(u orderByUniqueKey, cl orderByClause, table string) (bool, string) {
	if u.partial == "" {
		return true, ""
	}
	if orderByPredicateEntailed(u.partial, cl.where, table, cl) {
		return true, ""
	}
	return false, fmt.Sprintf(
		"%s 是部分唯一索引（谓词 %q），但本条 SQL 的 WHERE 里没有这个谓词 —— 部分索引不生效，不能当唯一性证据",
		u.evidence, u.partial)
}

// orderByPredicateEntailed 报告部分唯一索引的谓词是否被本条 SQL 的 WHERE 蕴含。
//
// 只认 `<col> IS [NOT] NULL` 这一种形态（schema 里的部分索引都是这一种），
// 其余形态返回 false（fail closed）。
func orderByPredicateEntailed(predicate, where, table string, cl orderByClause) bool {
	m := orderByPartialPredicateRe.FindStringSubmatch(predicate)
	if m == nil {
		return false
	}
	col := strings.ToLower(m[1])
	negated := strings.TrimSpace(m[2]) != ""
	// 带限定符的写法：限定符必须指向同一张表（别的表的同名列不算）。
	qualified := regexp.MustCompile(`([a-z_][a-z0-9_]*)\.` + regexp.QuoteMeta(col) + `\s+is\s+(not\s+)?null`)
	for _, qm := range qualified.FindAllStringSubmatch(where, -1) {
		if (strings.TrimSpace(qm[2]) != "") != negated {
			continue
		}
		if resolved, ok := cl.qualifiers[strings.ToLower(qm[1])]; ok && resolved == table {
			return true
		}
	}
	// 不带限定符的写法：要求 where 里确实出现裸 `col is [not] null`（前面不是点号）。
	nullness := "null"
	if negated {
		nullness = "not null"
	}
	bare := regexp.MustCompile(`(?:^|[^a-z0-9_.])` + regexp.QuoteMeta(col) + `\s+is\s+` + nullness)
	return bare.MatchString(where)
}

// orderByColumnsAllIn 报告 cols 是否全部落在 set 里。
func orderByColumnsAllIn(cols []string, set map[string]bool) bool {
	for _, c := range cols {
		if !set[c] {
			return false
		}
	}
	return true
}

// orderByColumnsNotInKeys 返回唯一键里**不在 ORDER BY 键里**的列（它们必须由 WHERE 钉住）。
func orderByColumnsNotInKeys(cols []string, keys []string) []string {
	keySet := map[string]bool{}
	for _, k := range keys {
		if col, _, ok := orderByNormalizeSortKey(k); ok {
			keySet[col] = true
		}
	}
	var out []string
	for _, c := range cols {
		if !keySet[c] {
			out = append(out, c)
		}
	}
	return out
}

// orderByClauseFailure 把一条未证明的 ORDER BY 渲染成「点名到行」的失败信息：
// 文件:行 + 表 + 子句原文 + 末键为什么不唯一 + 修法建议。
func orderByClauseFailure(cl orderByClause, sch *orderBySchema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "internal/store/%s:%d · 表 %s", cl.file, cl.line, orderByTableLabel(cl))
	fmt.Fprintf(&b, "\n    子句：%s", cl.clause)
	for _, p := range cl.problems {
		fmt.Fprintf(&b, "\n    解析：%s", p)
	}
	if len(cl.unresolved) > 0 {
		fmt.Fprintf(&b, "\n    解析：SQL 里有 %d 处拼不出来的子表达式（%s）——表名/条件可能因此判不出",
			len(cl.unresolved), strings.Join(cl.unresolved, "、"))
	}
	if tbl := sch.tables[cl.table]; tbl != nil {
		var ev []string
		for _, u := range tbl.uniques {
			e := u.evidence
			if u.partial != "" {
				e += "（部分，WHERE " + u.partial + "）"
			}
			ev = append(ev, e)
		}
		fmt.Fprintf(&b, "\n    %s 的唯一键：%s", cl.table, strings.Join(ev, "；"))
	}
	fmt.Fprintf(&b, "\n    修法：%s", orderByFixSuggestion(cl, sch))
	return b.String()
}

// orderByTableLabel 给失败信息用的表名（解析不出时说明是解析问题）。
func orderByTableLabel(cl orderByClause) string {
	if cl.table == "" {
		return "（未解析出表名）"
	}
	return cl.table
}

// orderByFixSuggestion 给出这条子句的修法建议。
func orderByFixSuggestion(cl orderByClause, sch *orderBySchema) string {
	tbl := sch.tables[cl.table]
	last := ""
	if len(cl.keys) > 0 {
		last = cl.keys[len(cl.keys)-1]
	}
	qual := ""
	if _, q, ok := orderByNormalizeSortKey(last); ok && q != "" {
		qual = q + "."
	}
	if tbl == nil {
		return "末键无法在本门禁下证明 —— 若它是 SQLite 目录表/pragma 虚表，登记进 orderByExemptions 并写清「顺序为什么无语义」；否则改成按唯一列排序"
	}
	// 优先 rowid：它是插入序，语义与 1461f7e4 的修法一致（「首注册用户排最前」只能这么表达）；
	// 没有 rowid 的表退到单列唯一键。
	suffix := "rowid"
	if !tbl.hasRowid {
		suffix = "主键列"
		for _, u := range tbl.uniques {
			if len(u.columns) == 1 {
				suffix = u.columns[0]
				break
			}
		}
	}
	// 用解析出来的排序键拼建议，不用子句原文：原文还带着 ` LIMIT ?`，拼出来的修法是错的。
	keysText := strings.Join(cl.keys, ", ")
	return fmt.Sprintf("末键后补一个唯一列：`ORDER BY %s, %s%s`（%s 的唯一键：%s）",
		keysText, qual, suffix, cl.table, orderByUniqueKeySummary(tbl))
}

// orderByUniqueKeySummary 把一张表的唯一键压成一行，用于失败信息。
func orderByUniqueKeySummary(tbl *orderByTable) string {
	var parts []string
	for _, u := range tbl.uniques {
		parts = append(parts, strings.Join(u.columns, "+"))
	}
	if len(parts) == 0 {
		return "（无）"
	}
	return strings.Join(parts, "、")
}

// ============================================================================
// 五、登记表：证明无害的白名单 / 已确认的缺口
// ============================================================================
//
// 两张表的纪律（照 internal/server/openapi_parity_test.go 的例外表）：
//   - 每条必须写**理由**与**可核验证据**（Evidence 要指得出看哪里，不许「应该没问题」）；
//   - 登记项过期即红：不再命中任何 ORDER BY 就失败，要求删除 —— 例外不许变成永久后门；
//   - 白名单只收「顺序真的没有语义」的，收不下的改进 orderByKnownGaps（缺口有上限预算）。
//
// 键是 `文件 + 子句原文（小写、空白压平）`：不用行号，避免改一行注释就整表失效；
// 同一文件里两条原文相同的子句共用一条登记（hits 会打印出来，多于 1 时必须核对两张表都安全）。

const (
	// orderByMinimumClauses 是「扫到的 ORDER BY 条数」下限，只许升不许降。
	//
	// 实测出处：2026-11-16 落地本门禁时，`internal/store/*.go`（非测试）里有 74 行文本含
	// `ORDER BY`，其中 4 行是注释（artifact_manifest.go:366、event_page.go:12、
	// playbook_tx.go:139、stats.go:113），真正写在 SQL 字符串里的是 **70** 条。
	// 下限取 69：留 1 条余量给「解析器对某种新写法暂不认识」，但少两条就要红 ——
	// 任何「扫描器悄悄不干活」都必须表现为失败，而不是静默变绿。
	orderByMinimumClauses = 69

	// orderByKnownGapBudget 是「已确认缺口」的条数上限（按子句计，不是按登记项计）。
	//
	// **只许降不许升**：修掉一条就把这里的数字与 orderByKnownGaps 对应登记一起下调；
	// 新缺口要通过本门禁只能走进 orderByExemptions（证明顺序无害），不许抬高预算蒙混。
	//
	// 12 → 11（2026-11-18）：ListUsersWithStats 与 1461f7e4 同形，
	// 补 `, u.rowid` 后登记项过期，按纪律下调预算（不跟着擦账，TestStoreOrderByTiebreak 会报「登记项过期」）。
	orderByKnownGapBudget = 11
)

// orderByExemption 是一条「证明顺序无语义」的白名单登记。
type orderByExemption struct {
	File     string // internal/store 下的文件名
	Clause   string // ORDER BY 子句原文（小写、空白压平；与扫描结果的 clauseKey 逐字相等）
	Reason   string // 为什么这对顺序不敏感
	Evidence string // 可核验证据：查哪里能确认（行号/函数名/不变量）
}

// orderByKnownGap 是一条**已确认为缺口**的登记：末键不唯一，顺序确实不确定，但本轮不修。
type orderByKnownGap struct {
	File   string
	Clause string
	Risk   string // 会怎么坏（谁来读这个顺序、并列时看到什么）
	Fix    string // 修法
}

// orderByExemptions 是白名单：只有「顺序真的没有语义」的才进得来。
var orderByExemptions = []orderByExemption{
	{
		File:   "migrate.go",
		Clause: "order by type, name",
		Reason: "SQLite 目录表的枚举：结果只用来判「库里有没有用户对象」与拼诊断文本，顺序不进入任何行为；" +
			"且 sqlite_master 的对象名在同一 schema 内唯一（表/视图/索引共享命名空间），并列不成立",
		Evidence: "migrate.go:276 userSchemaObjects 的唯一消费者是 migrate.go:335 `len(f.objects) > 0` 与 " +
			"migrate.go:339 的错误消息 strings.Join；没有任何按顺序取用的地方",
	},
	{
		File:   "migrate.go",
		Clause: "order by name",
		Reason: "同上，只取 `type='table'` 的表名：结果喂给 map/ledger 探测，顺序无语义；" +
			"表名在 sqlite_master 内唯一（本例还额外用 type='table' 收窄）",
		Evidence: "migrate.go:244-258（ledger 探测是 for 循环里的相等比较）、migrate.go:519-531（写进 map[表名]）",
	},
	{
		File:   "migrate.go",
		Clause: "order by id, seq",
		Reason: "pragma_foreign_key_list 是 SQLite 虚表（不在 schema.sql 管辖）：行按 id 分组拼进 map，" +
			"最后 out 又被 sort.Strings 显式排序 —— 顺序在消费端被重新确定",
		Evidence: "migrate.go:606-628：groups[id] 累积，末尾 `sort.Strings(out)`",
	},
	{
		File:   "migrate.go",
		Clause: "order by seq",
		Reason: "pragma_index_list 是 SQLite 虚表：结果只用于按列匹配（嵌套循环逐列比对唯一索引），" +
			"顺序不参与判定",
		Evidence: "migrate.go:448-456：for actual := range got.unique / for required := range want[...]，按列比较",
	},
	{
		File:     "migrate.go",
		Clause:   "order by seqno",
		Reason:   "pragma_index_xinfo 是 SQLite 虚表：seqno 是列在索引里的位置，同一索引内唯一 —— 不存在并列",
		Evidence: "migrate.go:680 indexKeyColumnsDB 只在按索引取列顺序时使用；SQLite 文档保证 seqno 为该 pivot 的行号",
	},
	{
		File:   "retention.go",
		Clause: "order by coalesce(t.result_accepted_at, case when t.reported=0 then t.finished_at end) limit ?",
		Reason: "清理批量选取，不是分页：批次边界不来自「上一批的最后一行」，而是调用方传入的固定 cutoff，" +
			"所以并列只影响「这一批先删哪几条」，到期条件与轮次无关（每轮重算，直到删空）；顺序无语义",
		Evidence: "retention.go:150-166：WHERE 里只有 `COALESCE(...) < ?`（cutoff 参数）与 NOT EXISTS 保护，没有基于排序键的续传条件",
	},
	{
		File:     "retention.go",
		Clause:   "order by e.created_at limit ?",
		Reason:   "同上的清理批量选取：到期判定是 `created_at < cutoff`，与上一批删到哪无关；并列只是本批选谁先删",
		Evidence: "retention.go:189-198：`DELETE FROM events WHERE id IN (SELECT e.id ... WHERE e.created_at < ? ... ORDER BY e.created_at LIMIT ?)`",
	},
	{
		File:     "retention.go",
		Clause:   "order by expires_at limit ?",
		Reason:   "同上的清理批量选取：`expires_at <= ?` 是固定阈值，不是游标；并列无语义",
		Evidence: "retention.go:218-225：`DELETE FROM event_deliveries WHERE rowid IN (SELECT rowid ... WHERE expires_at > 0 AND expires_at <= ? ... LIMIT ?)`",
	},
	{
		File:     "retention.go",
		Clause:   "order by pi.created_at limit ?",
		Reason:   "同上的清理批量选取：`pi.created_at < ?` 是固定阈值 + NOT EXISTS 引用保护（保护每轮重算），并列无语义",
		Evidence: "retention.go:255-268：批内按 created_at 排序只为「先删更老的」，并列时先删哪条不影响最终结果",
	},
}

// orderByKnownGaps 是已确认的缺口（本轮不修，由协调者裁决）。
//
// 每一条都真的会坏：末键不唯一 ⇒ 并列行的先后由查询计划/插入序决定 ⇒
// 「谁排第一」「LIMIT 边界取到谁」不是不变量。1461f7e4 修的是其中一条的分页版
// （ListUsersWithStatsPage → ORDER BY created_at, rowid），本表是同一类问题的存量清单。
var orderByKnownGaps = []orderByKnownGap{
	{
		File:   "admin.go",
		Clause: "order by coalesce((select max(s2.last_seen) from sessions s2 where s2.user_id = u.id), 0) desc, u.created_at asc limit 10",
		Risk:   "系统统计的 Top 10 活跃用户：末键 created_at 可重复，并列时谁进 Top10、谁排第几由计划决定（LIMIT 边界抖动）",
		Fix:    "末键补 u.rowid：ORDER BY COALESCE(...) DESC, u.created_at ASC, u.rowid ASC",
	},
	{
		File:   "admin.go",
		Clause: "order by created_at desc limit ?",
		Risk:   "ListGlobalEvents（admin_handlers.go:347）：同秒事件并列 → 「最近 N 条全站事件」的边界与内部顺序不确定",
		Fix:    "ORDER BY created_at DESC, id DESC（事件 id 唯一）",
	},
	{
		File:   "admin.go",
		Clause: "order by s.last_seen desc limit 500",
		Risk:   "ListAllSessions（admin_handlers.go:357）：last_seen 是秒级时间戳，同秒会话并列 → 500 条边界与顺序不确定",
		Fix:    "ORDER BY s.last_seen DESC, s.id DESC",
	},
	{
		File:   "auth.go",
		Clause: "order by created_at asc",
		Risk: "ListPasskeysByUser（auth.go:208，passkeys）与 ListUnusedRecoveryCodesByUser（auth.go:600，recovery_codes）：" +
			"同一秒创建的多条凭证/恢复码并列，列表顺序不确定（用户看到的「第 N 个凭证」会变）",
		Fix: "两条都补唯一列：ORDER BY created_at ASC, id ASC",
	},
	{
		File:   "auth.go",
		Clause: "order by created_at desc",
		Risk: "ListSessionsByUser（auth.go:355，sessions）与 ListAPIKeysByUser（auth.go:531，api_keys）：同秒并列 → 列表顺序不确定" +
			"（分页版 auth.go:421/382 已用 `created_at, id` 破平局，非分页版漏了）",
		Fix: "两条都补唯一列：ORDER BY created_at DESC, id DESC",
	},
	{
		File:   "deployment.go",
		Clause: "order by d.node_id",
		Risk: "按能力列部署（deployment.go:279）：deployments 的唯一键是 (node_id, capability_id, digest)，" +
			"同一节点对同一能力可以有多个 digest → node_id 并列，返回顺序不确定",
		Fix: "ORDER BY d.node_id, d.digest（或 d.id）",
	},
	{
		File:   "playbook.go",
		Clause: "order by name, version",
		Risk: "install 回执的 kept 能力清单（playbook.go:608）：WHERE **没有** deleted_at IS NULL，" +
			"部分唯一索引 idx_cap_live_name_ver(namespace_id,name,version) 不生效 → 已删能力的 (name,version) 可与活行重复，" +
			"顺序与内容都不确定（顺带把已删能力算进 kept？这是产品口径问题，不是本门禁能定的）",
		Fix: "补上 `AND deleted_at IS NULL`（同文件 playbook_tx.go:452-456 就是这么写的），再 ORDER BY name, version",
	},
}

// orderByMatchExemption 在一张白名单表里找命中项（-1 = 没命中）。
//
// 表作为参数传进来（不直接读全局）：变异自证要能喂一条假登记进去断言会红。
func orderByMatchExemption(cl orderByClause, exs []orderByExemption) int {
	for i, ex := range exs {
		if ex.File == cl.file && ex.Clause == cl.clauseKey {
			return i
		}
	}
	return -1
}

// orderByMatchGap 在一张缺口登记表里找命中项（-1 = 没命中）。
func orderByMatchGap(cl orderByClause, gaps []orderByKnownGap) int {
	for i, gap := range gaps {
		if gap.File == cl.file && gap.Clause == cl.clauseKey {
			return i
		}
	}
	return -1
}

// orderByScanGuard 是「解析器必须真的干活」的守卫（反假绿），单独成函数便于变异自证。
func orderByScanGuard(count, minimum int) []string {
	if count == 0 {
		return []string{"解析到 0 条 ORDER BY —— 扫描器坏了或被绕过；本门禁在 0 条上必须红，不许静默通过"}
	}
	if count <= minimum {
		return []string{fmt.Sprintf(
			"只解析到 %d 条 ORDER BY，下限是 > %d（实测 70 条，见 orderByMinimumClauses 注释）："+
				"少这么多说明扫描漏了，不是「代码变干净了」", count, minimum)}
	}
	return nil
}

// orderByRegistryProblems 检查两张登记表：过期即红（不再命中任何未证明的 ORDER BY）+ 缺口预算。
//
// 单独成函数（不内联在 Test 里）是为了让变异自证能直接喂一条假登记进去断言会红。
func orderByRegistryProblems(unproven []orderByClause, exs []orderByExemption, gaps []orderByKnownGap, budget int) []string {
	var problems []string
	exHits := make([]int, len(exs))
	gapHits := make([]int, len(gaps))
	for _, cl := range unproven {
		if i := orderByMatchExemption(cl, exs); i >= 0 {
			exHits[i]++
		}
		if i := orderByMatchGap(cl, gaps); i >= 0 {
			gapHits[i]++
		}
	}
	for i, ex := range exs {
		if exHits[i] == 0 {
			problems = append(problems, fmt.Sprintf(
				"白名单已过期：internal/store/%s 的 `%s` 不再命中任何未证明的 ORDER BY（这条子句可能已被修好/被证明）——"+
					"请从 orderByExemptions 删除该条（原理由：%s）", ex.File, ex.Clause, ex.Reason))
		}
	}
	total := 0
	for i, gap := range gaps {
		total += gapHits[i]
		if gapHits[i] == 0 {
			problems = append(problems, fmt.Sprintf(
				"缺口登记已过期：internal/store/%s 的 `%s` 不再命中任何未证明的 ORDER BY（缺口已修好或被证明）——"+
					"请从 orderByKnownGaps 删除该条，并把 orderByKnownGapBudget 从 %d 下调（原风险：%s）",
				gap.File, gap.Clause, budget, gap.Risk))
		}
	}
	if total > budget {
		problems = append(problems, fmt.Sprintf(
			"已确认缺口 %d 条，超出预算 %d：orderByKnownGapBudget 只许降不许升 —— "+
				"要么修掉（多半是末键补一个唯一列），要么证明顺序无害后进 orderByExemptions", total, budget))
	}
	return problems
}

// ============================================================================
// 六、门禁本体
// ============================================================================

// orderByGateStats 是一次门禁判定的汇总（打印用 + 变异自证断言用）。
type orderByGateStats struct {
	byClass   map[string]int
	exempted  int
	knownGaps int // 命中的**子句**条数（不是登记项数）
	// unproven 是判据证不出来的子句（登记表的命中情况在它之上定义）。
	unproven []orderByClause
	problems []string
}

// orderByGate 是门禁本体（纯函数：输入子句表 + schema + 登记表，输出证据/统计/问题）。
//
// 从 Test 里抽出来是为了让「红侧」也能自证：变异自证可以喂一条末键非唯一的子句进来，
// 断言它被按 文件:行 点名，而不是只测内层判据（orderByJudge）就算完。
func orderByGate(clauses []orderByClause, sch *orderBySchema, exs []orderByExemption, gaps []orderByKnownGap, budget int) ([]string, orderByGateStats) {
	stats := orderByGateStats{byClass: map[string]int{}}
	var details []string
	var unproven []orderByClause
	// unprovenProblems 与 unproven 同下标对齐：证不出来的原因要原样留在失败信息里，
	// 不重跑一遍判据（判据是纯函数，重跑只是浪费）。
	var unprovenProblems [][]string
	for _, cl := range clauses {
		proof, judgeProblems := orderByJudge(cl, sch)
		if proof.class != "" {
			stats.byClass[proof.class]++
			details = append(details, fmt.Sprintf("✅ %s internal/store/%s:%d · %s",
				proof.class, cl.file, cl.line, proof.evidence))
			continue
		}
		unproven = append(unproven, cl)
		unprovenProblems = append(unprovenProblems, judgeProblems)
	}
	stats.unproven = unproven
	for i, cl := range unproven {
		exIdx, gapIdx := orderByMatchExemption(cl, exs), orderByMatchGap(cl, gaps)
		switch {
		case exIdx >= 0 && gapIdx >= 0:
			stats.problems = append(stats.problems, fmt.Sprintf(
				"internal/store/%s:%d 同时登记在白名单与缺口表 —— 一条子句只能是一者", cl.file, cl.line))
		case exIdx >= 0:
			stats.exempted++
			details = append(details, fmt.Sprintf("🟡 白名单 internal/store/%s:%d · %s",
				cl.file, cl.line, exs[exIdx].Reason))
		case gapIdx >= 0:
			stats.knownGaps++
			details = append(details, fmt.Sprintf("🔴 已知缺口 internal/store/%s:%d · %s",
				cl.file, cl.line, gaps[gapIdx].Risk))
		default:
			stats.problems = append(stats.problems, orderByClauseFailure(cl, sch)+
				"\n    判据："+strings.Join(unprovenProblems[i], "；"))
		}
	}
	// ④ 两张登记表过期即红 + 缺口预算只许降。
	stats.problems = append(stats.problems, orderByRegistryProblems(stats.unproven, exs, gaps, budget)...)
	return details, stats
}

// TestStoreOrderByTiebreak 是 L4 门禁：store 里每条 ORDER BY 的末键必须唯一。
//
// 失败信息的形态（直接给修法，省掉一次「这是哪条子句」的往返）：
//
//	internal/store/admin.go · 表 users	← 行号随源码变，下面取立项时的一条
//	    子句：ORDER BY u.created_at ASC
//	    users 的唯一键：列级 PRIMARY KEY（id）；列级 UNIQUE（username）
//	    修法：末键后补一个唯一列：`ORDER BY u.created_at ASC, rowid`（users 的唯一键：id、username）
func TestStoreOrderByTiebreak(t *testing.T) {
	clauses := orderByScanClauses(t)
	sch := orderByLoadSchema(t)

	// ⑤ 反假绿：0 条 / 低于下限都必须红。
	if guard := orderByScanGuard(len(clauses), orderByMinimumClauses); len(guard) > 0 {
		t.Fatalf("%s", strings.Join(guard, "\n"))
	}

	details, stats := orderByGate(clauses, sch, orderByExemptions, orderByKnownGaps, orderByKnownGapBudget)
	for _, d := range details {
		t.Log(d)
	}
	t.Logf("ORDER BY 共 %d 条（下限 > %d）｜末键唯一 %d｜分组键 %d｜覆盖唯一键 %d｜白名单 %d｜已知缺口 %d（预算上限 %d）｜"+
		"登记：白名单 %d 条 / 缺口 %d 条",
		len(clauses), orderByMinimumClauses, stats.byClass["末键唯一"], stats.byClass["分组键"], stats.byClass["覆盖唯一键"],
		stats.exempted, stats.knownGaps, orderByKnownGapBudget, len(orderByExemptions), len(orderByKnownGaps))

	if len(stats.problems) > 0 {
		t.Fatalf("ORDER BY 破平局门禁失败（%d 条）：\n  - %s", len(stats.problems), strings.Join(stats.problems, "\n  - "))
	}

	// ========================================================================
	// ⑦ 变异自证：判据必须真的会因为「末键不唯一」变红，而不是恒绿。
	// ========================================================================
	t.Run("schema 解析自证：唯一键与部分索引谓词确实解析到了", func(t *testing.T) {
		// 判据全靠这些事实；解析器悄悄退化成「什么都没有」时，下面的断言会先红。
		users := sch.tables["users"]
		if users == nil || !users.hasRowid {
			t.Fatalf("users 表解析异常：%+v（它是普通 rowid 表）", users)
		}
		if !orderByColumnsAllIn([]string{"id"}, orderByUniqueColumnSet(users)) {
			t.Fatalf("users 的单列唯一键 id 没解析出来：%+v", users.uniques)
		}
		if orderByColumnsAllIn([]string{"created_at"}, orderByUniqueColumnSet(users)) {
			t.Fatalf("users.created_at 被当成了唯一键 —— 解析器把非唯一列也算进唯一性事实里了")
		}
		capTbl := sch.tables["capability_versions"]
		if capTbl == nil {
			t.Fatalf("capability_versions 表没解析出来")
		}
		var partial bool
		for _, u := range capTbl.uniques {
			if len(u.columns) == 3 && u.columns[0] == "namespace_id" && u.columns[2] == "version" && u.partial != "" {
				partial = true
			}
		}
		if !partial {
			t.Fatalf("idx_cap_live_name_ver 没被识别成「带 WHERE 谓词的部分唯一索引」：%+v", capTbl.uniques)
		}
	})

	t.Run("变异：把 rowid 破平局换成同列（非唯一）→ 必须红", func(t *testing.T) {
		cl := orderByRequireClause(t, clauses, "admin.go", "order by u.created_at asc, u.rowid asc limit ?")
		if proof, _ := orderByJudge(cl, sch); proof.class == "" {
			t.Fatalf("绿侧不成立：原始子句应当已被证明（%s）", cl.clause)
		}
		mutated := orderByReanalyze(t, cl, func(sqlText string) string {
			return strings.Replace(sqlText, "u.rowid ASC", "u.created_at ASC", 1)
		})
		proof, judgeProblems := orderByJudge(mutated, sch)
		if proof.class != "" {
			t.Fatalf("变异没被抓到：末键换成非唯一的 u.created_at 之后仍被判成 %q（%s）", proof.class, proof.evidence)
		}
		orderByRequireProblem(t, "rowid→created_at 的变异", strings.Join(judgeProblems, "；"), "末键 created_at 在 users 上不唯一")
	})

	t.Run("变异：去掉 deleted_at IS NULL → 部分唯一索引失效，必须红", func(t *testing.T) {
		// capability.go:557 靠部分唯一索引 idx_cap_live_name_ver 证明 (name, version) 在全表唯一；
		// 一旦 WHERE 不再带谓词，已删能力的同 name/version 行会让 version 重新可重复。
		cl := orderByRequireClause(t, clauses, "capability.go", "order by name, version")
		if proof, _ := orderByJudge(cl, sch); proof.class == "" {
			t.Fatalf("绿侧不成立：原始子句应当已由部分唯一索引证明（%s）", cl.clause)
		}
		mutated := orderByReanalyze(t, cl, func(sqlText string) string {
			return strings.Replace(sqlText, "AND deleted_at IS NULL", "", 1)
		})
		proof, judgeProblems := orderByJudge(mutated, sch)
		if proof.class != "" {
			t.Fatalf("变异没被抓到：去掉谓词后仍被判成 %q", proof.class)
		}
		orderByRequireProblem(t, "删掉 deleted_at IS NULL 的变异", strings.Join(judgeProblems, "；"), "部分唯一索引")
	})

	t.Run("变异：去掉 WHERE 里钉住的 namespace_id → 复合主键覆盖不上，必须红", func(t *testing.T) {
		// graph.go:85 靠「edges 复合主键 (namespace_id, from_is_source, from_ref, to_ref)
		// 被 ORDER BY 全键 + WHERE 钉住的 namespace_id 覆盖」。钉住消失后必须立刻红 ——
		// 否则「覆盖唯一键」这条规则就是「键多就算安全」的假绿。
		cl := orderByRequireClause(t, clauses, "graph.go", "order by from_is_source, from_ref, to_ref")
		if proof, _ := orderByJudge(cl, sch); proof.class != "覆盖唯一键" {
			t.Fatalf("绿侧不成立：原始子句应当由覆盖唯一键证明，实际 %q", proof.class)
		}
		mutated := orderByReanalyze(t, cl, func(sqlText string) string {
			return strings.Replace(sqlText, "WHERE namespace_id = ?", "WHERE 1 = 1", 1)
		})
		proof, judgeProblems := orderByJudge(mutated, sch)
		if proof.class != "" {
			t.Fatalf("变异没被抓到：去掉 namespace_id 钉住后仍被判成 %q（%s）", proof.class, proof.evidence)
		}
		orderByRequireProblem(t, "去掉 namespace_id 钉住的变异", strings.Join(judgeProblems, "；"), "末键 to_ref 在 edges 上不唯一")
	})

	t.Run("变异：扫描条数归零 → 反假绿守卫必须红", func(t *testing.T) {
		if guard := orderByScanGuard(0, orderByMinimumClauses); len(guard) == 0 {
			t.Fatalf("0 条 ORDER BY 竟然通过了守卫 —— 门禁可以在「什么都没扫到」时变绿，等于没有门禁")
		}
		if guard := orderByScanGuard(orderByMinimumClauses, orderByMinimumClauses); len(guard) == 0 {
			t.Fatalf("条数等于下限（不 > 下限）竟然通过了守卫")
		}
		if guard := orderByScanGuard(len(clauses), orderByMinimumClauses); len(guard) > 0 {
			t.Fatalf("绿侧不成立：实测 %d 条应当通过下限守卫，实际 %v", len(clauses), guard)
		}
	})

	t.Run("变异：登记表过期 → 必须红（例外不许变成永久后门）", func(t *testing.T) {
		// 红侧：一条永远命中不了的登记必须被点名要求删除。
		probe := []orderByExemption{{
			File: "admin.go", Clause: "order by u.id",
			Reason: "变异用：多余的登记", Evidence: "变异用：本门禁里不存在这条子句",
		}}
		if problems := orderByRegistryProblems(stats.unproven, probe, nil, orderByKnownGapBudget); !orderByHasProblem(problems, "白名单已过期") {
			t.Fatalf("过期白名单没被抓到：%v", problems)
		}
		// 红侧：预算一旦收紧到缺口数以下，就必须报超预算。
		if len(orderByKnownGaps) > 0 {
			if problems := orderByRegistryProblems(stats.unproven, orderByExemptions, orderByKnownGaps, 0); !orderByHasProblem(problems, "超出预算") {
				t.Fatalf("缺口预算收紧到 0 之后没报超预算：%v", problems)
			}
		}
		// 绿侧：真实登记表 + 真实未证明集合，一条问题都不许有。
		if problems := orderByRegistryProblems(stats.unproven, orderByExemptions, orderByKnownGaps, orderByKnownGapBudget); len(problems) > 0 {
			t.Fatalf("绿侧不成立：真实登记表自己报问题 %v", problems)
		}
	})

	t.Run("变异：JOIN 的 ON 换成非唯一列 → 唯一性证明必须作废", func(t *testing.T) {
		// deployment.go:425 的末键 d.id 是 deployments 的主键，但那条 SQL JOIN 了 capability_versions。
		// 「末键唯一」只有在 JOIN 至多 1:1 时才蕴含「结果集每行只出现一次」——
		// 把 ON 换成不唯一的列之后，证明必须立刻作废（fail closed，不许一边报问题一边算通过）。
		cl := orderByRequireClause(t, clauses, "deployment.go", "order by d.node_id, c.version, d.id")
		if proof, _ := orderByJudge(cl, sch); proof.class != "末键唯一" {
			t.Fatalf("绿侧不成立：原始子句应当由末键唯一证明，实际 %q", proof.class)
		}
		mutated := orderByReanalyze(t, cl, func(sqlText string) string {
			return strings.Replace(sqlText, "JOIN capability_versions c ON c.id = d.capability_id",
				"JOIN capability_versions c ON c.namespace_id = d.capability_id", 1)
		})
		proof, judgeProblems := orderByJudge(mutated, sch)
		if proof.class != "" {
			t.Fatalf("变异没被抓到：JOIN 的 ON 列不唯一（会放大行数）时仍被判成 %q（%s）", proof.class, proof.evidence)
		}
		orderByRequireProblem(t, "JOIN ON 换成 namespace_id 的变异", strings.Join(judgeProblems, "；"), "不构成唯一键")
	})

	t.Run("变异：未登记的非唯一末键 → 门禁必须按 文件:行 点名失败", func(t *testing.T) {
		// 把真实子句换成「末键非唯一」的变异体，走**整条门禁**（不是只测内层判据）：
		// 断言失败信息指名道姓（文件:行 + 表 + 子句原文 + 末键为什么不唯一 + 修法），
		// 并且不会被任何登记表兜住。
		var mutated []orderByClause
		// 期望的「文件:行」从扫描结果现算：本测试上面的注释/代码改一行，行号就变，
		// 写死字面量会把「判据其实没点名」伪装成自测失败（2026-11-18 真实发生）。
		wantSite := ""
		for _, cl := range clauses {
			if cl.file == "admin.go" && cl.clauseKey == "order by u.created_at asc, u.rowid asc limit ?" {
				wantSite = fmt.Sprintf("internal/store/%s:%d", cl.file, cl.line)
				mutated = append(mutated, orderByReanalyze(t, cl, func(sqlText string) string {
					return strings.Replace(sqlText, "u.rowid ASC", "u.created_at ASC", 1)
				}))
				continue
			}
			mutated = append(mutated, cl)
		}
		if wantSite == "" {
			t.Fatalf("变异目标子句不见了（admin.go 的分页 ORDER BY 改了原文？先修本自测的目标定位）")
		}
		_, got := orderByGate(mutated, sch, orderByExemptions, orderByKnownGaps, orderByKnownGapBudget)
		var hit string
		for _, p := range got.problems {
			if strings.Contains(p, wantSite) {
				hit = p
			}
		}
		if hit == "" {
			t.Fatalf("整条门禁没抓到变异（问题列表 %v）", got.problems)
		}
		for _, want := range []string{"表 users", "ORDER BY u.created_at ASC", "末键 created_at 在 users 上不唯一", "修法："} {
			if !strings.Contains(hit, want) {
				t.Fatalf("失败信息缺 %q，实际：\n%s", want, hit)
			}
		}
		// 绿侧：换回原来的子句，同一个门禁必须一条问题都没有。
		if _, ok := orderByGate(clauses, sch, orderByExemptions, orderByKnownGaps, orderByKnownGapBudget); len(ok.problems) > 0 {
			t.Fatalf("绿侧不成立：未变异的子句集应当通过，实际 %v", ok.problems)
		}
	})
}

// orderByUniqueColumnSet 把一张表的单列唯一键收成集合（供自证用）。
func orderByUniqueColumnSet(tbl *orderByTable) map[string]bool {
	set := map[string]bool{}
	for _, u := range tbl.uniques {
		if len(u.columns) == 1 {
			set[u.columns[0]] = true
		}
	}
	return set
}

// orderByRequireClause 按 文件 + 子句原文取一条扫描结果（取不到就失败）。
func orderByRequireClause(t *testing.T, clauses []orderByClause, file, clauseKey string) orderByClause {
	t.Helper()
	for _, cl := range clauses {
		if cl.file == file && cl.clauseKey == clauseKey {
			return cl
		}
	}
	t.Fatalf("扫描结果里找不到 internal/store/%s 的 `%s` —— 子句被改写了？变异自证依赖它", file, clauseKey)
	return orderByClause{}
}

// orderByReanalyze 在内存里用**变异后的整条 SQL** 重跑分析（源码文件一个字都不动）。
//
// 定位方式：变异只改 WHERE/其它部分，ORDER BY 子句原文不变 —— 按「规范化后的子句原文相等」
// 找到同一处 ORDER BY，再用新文本重新分析。
func orderByReanalyze(t *testing.T, cl orderByClause, mutate func(string) string) orderByClause {
	t.Helper()
	mutated := mutate(cl.sql)
	if mutated == cl.sql {
		t.Fatalf("变异没有真的改到 SQL 文本（internal/store/%s:%d）", cl.file, cl.line)
	}
	// 锚点：把原 ORDER BY 的偏移映射到变异后的文本上（变异在它前面就把偏移平移）。
	if off := orderByMapOffset(cl.sql, mutated, cl.orderOffset); off >= 0 &&
		strings.HasPrefix(strings.ToLower(mutated[off:]), "order by") {
		return orderByAnalyzeClause(orderByMutatedText(cl.file, mutated), orderByOccurrence{
			file: cl.file, line: cl.line, col: 1, offset: off, clause: orderByClauseText(mutated, off),
		})
	}
	// 回退：偏移映射不上（变异把 ORDER BY 前面的结构改得面目全非）时按子句原文找。
	lower := strings.ToLower(mutated)
	for from := 0; ; {
		idx := strings.Index(lower[from:], "order by")
		if idx < 0 {
			break
		}
		off := from + idx
		from = off + len("order by")
		if off > 0 && isOrderByWordByte(lower[off-1]) {
			continue
		}
		clause := orderByClauseText(mutated, off)
		if orderByNormalizeClause(clause) != cl.clauseKey {
			continue
		}
		return orderByAnalyzeClause(orderByMutatedText(cl.file, mutated), orderByOccurrence{
			file: cl.file, line: cl.line, col: 1, offset: off, clause: clause,
		})
	}
	t.Fatalf("变异后的 SQL 里找不到同一处 ORDER BY（%s）", cl.clauseKey)
	return orderByClause{}
}

// orderByMutatedText 把内存里变异过的 SQL 包成 orderByText（片段行号沿用原文件，仅用于报错定位）。
func orderByMutatedText(file, text string) orderByText {
	return orderByText{chunks: []orderByChunk{{file: file, line: 1, col: 1, text: text}}}
}

// orderByMapOffset 把旧文本里的偏移映射到新文本：
// 变异发生在偏移之前 → 偏移按长度差平移；发生在偏移之后（或子句内部）→ 偏移不动。
func orderByMapOffset(oldText, newText string, offset int) int {
	limit := len(oldText)
	if len(newText) < limit {
		limit = len(newText)
	}
	common := 0
	for common < limit && oldText[common] == newText[common] {
		common++
	}
	if common >= offset {
		if offset > len(newText) {
			return -1
		}
		return offset
	}
	moved := offset + (len(newText) - len(oldText))
	if moved < 0 || moved > len(newText) {
		return -1
	}
	return moved
}

// orderByRequireProblem 断言问题串里出现了期望的判据文本（变异真的被抓到）。
func orderByRequireProblem(t *testing.T, mutation, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("变异「%s」没有被判据抓住：期望出现 %q，实际判据 %q", mutation, want, got)
	}
}

// orderByHasProblem 断言问题串里出现了期望的子串。
func orderByHasProblem(problems []string, want string) bool {
	for _, p := range problems {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}
