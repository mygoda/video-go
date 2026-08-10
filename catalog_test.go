package aigcpool_test

import (
	"encoding/json"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	aigcpool "github.com/aigc-pool/aigc-pool"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// jsonLiteral 匹配迁移里单引号包住的 JSON 对象字面量。
//
// 迁移里所有 '{...}' 都是 JSON（capability、request_mapping、skills 的
// default_params），没有任何一处在字符串里带单引号——本文件的
// TestMigrationJSONLiteralsAreParsable 就是用来保证这条前提不被将来的迁移打破的：
// 一旦有人写了带单引号的 JSON，这个正则会切在半路上、切出来的东西解不开，
// 那条测试立刻红，而不是让下面的目录测试悄悄少覆盖一个模型。
var jsonLiteral = regexp.MustCompile(`(?s)'(\{.*?\})'`)

// whereID 取出「只改 request_mapping、不重写 capability」那类 UPDATE 的目标模型。
var whereID = regexp.MustCompile(`WHERE\s+id\s*=\s*'([^']+)'`)

// seededModel 是从迁移里还原出来的一行模型配置的最终状态。
type seededModel struct {
	id      string
	source  string // 最后写它的那个迁移文件，报错时指路用
	schema  capability.ModelCapabilitySchema
	mapping mapping.RequestMapping
	mapped  bool // request_mapping 是否为 NULL（compose 那类本地驱动不走 mapping）
}

// TestSeededModelsParamCoverage 把「capability.params 与 request_mapping.rules
// 一一对应」这条不变量钉在**真实播下去的模型目录**上。
//
// # 为什么要在这一层再钉一次
//
// mapping.ValidateParamCoverage 的单测证明的是判定函数本身对；这个测试证明的是
// 我们实际发出去的那份配置满足它。两者缺一不可——判定函数写得再对，没人拿它去量
// 迁移里的 JSON，配置照样能带着「参数到不了上游」出厂。
//
// 而这类错在运行期是**完全静默**的：控件照渲染、计价照算、上游照回 200，只是
// 那个参数从来没生效过（见 ValidateParamCoverage 的头注释）。它只可能在这里被拦。
//
// # 为什么读 SQL 而不是连库
//
// 连库跑迁移再查表更贴近真相，但那样这条测试就只在配了 AIGC_TEST_MYSQL_DSN 的
// 机器上跑（store/mysql 的集成测试就是这么做的，没 DSN 一律 t.Skip）。一条会
// skip 的不变量测试等于没有：它拦不住任何一次本地提交。迁移 SQL 是这份目录的
// 唯一真相来源，直接读它，任何一台机器上 `go test ./...` 都能跑。
func TestSeededModelsParamCoverage(t *testing.T) {
	models := loadSeededModels(t)
	if len(models) == 0 {
		t.Fatal("没有从迁移里还原出任何模型，提取器坏了")
	}

	for _, m := range models {
		t.Run(m.id, func(t *testing.T) {
			if !m.mapped {
				// request_mapping 为 NULL 的模型走本地驱动（local-compose-1），
				// 不经过 mapping 包。它只要不声明参数就仍然自洽。
				if n := len(capability.LeafParams(m.schema.Params)); n > 0 {
					t.Fatalf("%s（%s）没有 request_mapping 却声明了 %d 个参数，这些取值到不了任何地方",
						m.id, m.source, n)
				}
				return
			}
			report(t, m, mapping.ValidateParamCoverage(m.schema, m.mapping))
		})
	}
}

// TestSeedanceFastFitsStoryboardBatch 钉住「分镜那一批任务提得进去」。
//
// 000013 用 JSON_SET 把 seedance-fast 的 max_concurrent_per_user 从 2 抬到 4，
// 因为分镜一次派 httpapi.storyboardDefaultShots（= 3）条任务，而这个上限连
// 排队中的任务一起算，2 会让第 3 个镜头在提交时就被 rate_limited 顶回来。
//
// 危险在于 JSON 列是**整值写入**：任何后来的迁移只要整块重写 capability，
// 就会把 000013 那一笔悄悄擦掉，没有报错、没有冲突，只有分镜从此少一个镜头。
// 000015 第一版就正是这么干的（照抄了 000007 的 2），本测试是那次的回归。
//
// 这里写死 3 而不是引用常量：storyboardDefaultShots 在 internal/httpapi 里未导出。
// 分镜真要改批量大小，000013 的注释是必经之地，那时连这个数一起改。
func TestSeedanceFastFitsStoryboardBatch(t *testing.T) {
	const storyboardBatch = 3

	for _, m := range loadSeededModels(t) {
		if m.id != "gpugeek-seedance-2-0-fast" {
			continue
		}
		if got := m.schema.Limits.MaxConcurrentPerUser; got < storyboardBatch {
			t.Fatalf("%s（%s）: max_concurrent_per_user = %d，装不下分镜一批的 %d 条任务；"+
				"多半是某个迁移整块重写 capability 时漏掉了 000013 抬上去的那一笔",
				m.id, m.source, got, storyboardBatch)
		}
		return
	}
	t.Fatal("迁移里没有 gpugeek-seedance-2-0-fast，分镜依赖的模型不见了")
}

// TestSeededModelsSchemaSelfConsistent 顺带把每份播下去的 capability 过一遍
// ValidateSchema。管理后台存 schema 时会走这一道，迁移直接 INSERT 绕过了它——
// 于是「后台存不进去的配置，迁移能播进去」，而两者播的是同一张表。
func TestSeededModelsSchemaSelfConsistent(t *testing.T) {
	v := capability.NewValidator(nil)
	for _, m := range loadSeededModels(t) {
		t.Run(m.id, func(t *testing.T) {
			report(t, m, v.ValidateSchema(m.schema))
		})
	}
}

// TestMigrationJSONLiteralsAreParsable 守住上面两个测试的提取前提：
// 单引号切出来的每一段都必须是完整的 JSON，且里面不能有 ";"——
// 前者保证 jsonLiteral 没切歪，后者保证按 ";" 切语句不会把一份 JSON 劈成两半。
// 这两条一旦被将来的迁移打破，本测试立刻红，而不是让目录测试悄悄少覆盖一个模型。
func TestMigrationJSONLiteralsAreParsable(t *testing.T) {
	for _, name := range migrationFiles(t, "") {
		body := readMigration(t, name)
		for _, m := range jsonLiteral.FindAllStringSubmatch(body, -1) {
			var probe any
			if err := json.Unmarshal([]byte(m[1]), &probe); err != nil {
				t.Errorf("%s: 单引号里的 JSON 字面量解不开，说明 jsonLiteral 切错了位置"+
					"（多半是某段 JSON 的字符串里出现了单引号）: %v", name, err)
				continue
			}
			if strings.Contains(m[1], ";") {
				t.Errorf("%s: JSON 字面量里出现了 \";\"，按语句切分会把它劈开，"+
					"loadSeededModels 需要换一种切法", name)
			}
		}
	}
}

func report(t *testing.T, m seededModel, errs []domain.FieldError) {
	t.Helper()
	for _, e := range errs {
		t.Errorf("%s（%s）: %s — %s", m.id, m.source, e.Key, e.Message)
	}
}

// loadSeededModels 按迁移顺序回放所有 *.up.sql，还原每个模型的最终配置。
//
// 不解析 SQL，只用两条本仓库迁移一直遵守的结构约定：
//
//   - 一条语句只写一个模型。语句以 ";" 结束，而 JSON 字面量里不含 ";"
//     （TestMigrationJSONLiteralsAreParsable 守着这条前提），所以按 ";" 切开
//     就得到「一个模型一段」。
//   - 段内的模型主键要么在 capability 自带的 "id" 里（INSERT，以及会重写
//     capability 的 UPDATE），要么在 WHERE id = '...' 里（只改 mapping 的 UPDATE）。
//
// 后面的迁移覆盖前面的，与 UPDATE / ON DUPLICATE KEY UPDATE 的效果一致；
// 只改 capability 不改 request_mapping 的迁移会保留先前的 mapping，
// 与库里的实际状态一致。
func loadSeededModels(t *testing.T) []seededModel {
	t.Helper()

	byID := map[string]seededModel{}
	for _, name := range migrationFiles(t, ".up.sql") {
		for _, stmt := range strings.Split(readMigration(t, name), ";") {
			var (
				id     string
				schema capability.ModelCapabilitySchema
				rm     mapping.RequestMapping
				hasCap bool
				hasMap bool
			)
			for _, lit := range jsonLiteral.FindAllStringSubmatch(stmt, -1) {
				raw := []byte(lit[1])
				var probe map[string]json.RawMessage
				if json.Unmarshal(raw, &probe) != nil {
					continue // 由 TestMigrationJSONLiteralsAreParsable 报
				}
				_, params := probe["params"]
				_, hasIDField := probe["id"]
				switch {
				case params && hasIDField:
					s, err := capability.DecodeSchema(raw)
					if err != nil {
						t.Fatalf("%s: capability 解码失败: %v", name, err)
					}
					schema, hasCap, id = s, true, s.ID
				case hasRules(probe):
					m, err := mapping.DecodeMapping(raw)
					if err != nil {
						t.Fatalf("%s: request_mapping 解码失败: %v", name, err)
					}
					rm, hasMap = m, true
				}
			}
			if id == "" {
				if m := whereID.FindStringSubmatch(stmt); m != nil {
					id = m[1]
				}
			}
			if id == "" || (!hasCap && !hasMap) {
				continue
			}

			cur := byID[id]
			cur.id, cur.source = id, name
			if hasCap {
				cur.schema = schema
			}
			if hasMap {
				cur.mapping, cur.mapped = rm, true
			}
			byID[id] = cur
		}
	}

	out := make([]seededModel, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func hasRules(probe map[string]json.RawMessage) bool {
	_, rules := probe["rules"]
	_, tmpl := probe["body_template"]
	return rules || tmpl
}

func migrationFiles(t *testing.T, suffix string) []string {
	t.Helper()
	entries, err := fs.ReadDir(aigcpool.MigrationsFS, aigcpool.MigrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if suffix != "" && !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out) // 文件名带零填充序号，字典序即迁移顺序
	return out
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(aigcpool.MigrationsFS, path.Join(aigcpool.MigrationsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
