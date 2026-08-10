package mapping

import (
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// paramPrefix 是 MappingRule.From 里指向平台参数的前缀。
const paramPrefix = "params."

// ValidateParamCoverage 校验一份 RequestMapping 与它所属模型的 capability
// 在**参数面**上一一对应：capability.params 的每个叶子 key 都有一条
// from: params.<key> 的规则，且每条 params.* 规则都指向一个真实存在的叶子。
//
// # 为什么这条不变量必须被机器钉死
//
// 两个方向坏掉的形态都是**静默的**，没有任何一层会报错：
//
//   - 声明了参数却没有规则：控件照常渲染、取值照常参与计价与校验，就是不进
//     上游请求体。用户拖动「重绘强度」，费用变了，出来的图一模一样。
//   - 规则指向不存在的参数：sourceValues 取到空值，OmitWhenEmpty 默认为真，
//     于是那个字段被安静地略过。上游拿到一份少了字段的请求，用它自己的默认值
//     生成，也照样回 200。
//
// 两者的共同点是「能跑通、结果不对」，而这类问题只在有人逐字比对 capability
// 与 request_mapping 时才会被发现——那正是机器该干的事。本包的存在前提是
// 「接一个新模型 = 一条配置，零代码」，配置写错就必须在配置层被拦下。
//
// # 只管 params，不管 inputs
//
// 输入槽的覆盖不能一刀切：local-compose-1 声明了 composed_from 槽而
// request_mapping 是 NULL——它走本地 compose 驱动，根本不经过本包。
// 参数这条没有这类例外，params 为空的模型天然满足。
//
// compound 自身不产生取值（见 capability.LeafParams），因此规则指向 compound
// 的 key 时单独报一条：这与「参数不存在」的修法完全不同，前者是改 from 指向
// 子字段，后者是改 key 或补声明。
func ValidateParamCoverage(schema capability.ModelCapabilitySchema, m RequestMapping) []domain.FieldError {
	var errs []domain.FieldError

	leaves := capability.LeafParams(schema.Params)
	known := make(map[string]struct{}, len(leaves))
	for _, spec := range leaves {
		known[spec.Key] = struct{}{}
	}
	compounds := map[string]struct{}{}
	collectCompoundKeys(schema.Params, compounds)

	mapped := make(map[string]struct{}, len(m.Rules))
	for i, rule := range m.Rules {
		key, ok := strings.CutPrefix(rule.From, paramPrefix)
		if !ok {
			continue
		}
		mapped[key] = struct{}{}
		if _, ok := known[key]; ok {
			continue
		}
		msg := "规则指向了 capability.params 里不存在的参数 " + key
		if _, isCompound := compounds[key]; isCompound {
			msg = key + " 是 compound，本身不产生取值，from 应指向它的某个子字段"
		}
		errs = append(errs, domain.FieldError{
			Key:     "rules[" + strconv.Itoa(i) + "].from",
			Message: msg,
		})
	}

	for i, spec := range leaves {
		if _, ok := mapped[spec.Key]; ok {
			continue
		}
		errs = append(errs, domain.FieldError{
			Key:     "params[" + strconv.Itoa(i) + "].key",
			Message: "参数 " + spec.Key + " 没有对应的映射规则，它的取值到不了上游",
		})
	}
	return errs
}

func collectCompoundKeys(params []capability.ParamSpec, out map[string]struct{}) {
	for _, p := range params {
		if p.Control != capability.ControlCompound {
			continue
		}
		out[p.Key] = struct{}{}
		collectCompoundKeys(p.Fields, out)
	}
}
