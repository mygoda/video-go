package adapter

import (
	"fmt"
	"sort"
	"sync"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// registry 是 Registry 的默认实现。
//
// 同步与异步分两张表而不是一张 map[string]Driver 再做类型断言:
// 一个名字在同步表里查不到就是"这个协议不走同步路径"，是个明确的答案；
// 断言失败则是个含糊的 false，排查时分不清是没注册还是注册错了族。
//
// 加锁是因为注册可能发生在启动之后（admin 热加载一个新驱动实例），
// 而查找一直在 worker goroutine 里并发发生。
type registry struct {
	mu    sync.RWMutex
	sync  map[string]SyncDriver
	async map[string]AsyncDriver
}

// NewRegistry 返回一个空注册表。
//
// **它不 import 任何 driver 包**——装配（哪些驱动、各自什么配置）是
// cmd/aigcd 的事。若在这里写死一串 Register，"接新供应商 = 新增一个 driver 包"
// 就会变成"新增一个包并回来改 adapter 包"，L0 反过来依赖了它的下游。
func NewRegistry() Registry {
	return &registry{
		sync:  map[string]SyncDriver{},
		async: map[string]AsyncDriver{},
	}
}

// Mutable 是可写的注册表，NewRegistry 的返回值实现了它。
//
// Registry 接口只读，是给 L2 / httpapi 用的——它们只该查不该改。
// 装配方（cmd/aigcd）用类型断言拿到本接口来注册，一次断言换来
// "上层拿到的是只读视图"这条约束。
type Mutable interface {
	Registry
	// Register 按 d.Name() 注册一个驱动。d 必须实现 SyncDriver 或 AsyncDriver，
	// 二者都不实现则返回错误。同名重复注册返回错误而不是静默覆盖:
	// 装配期两个驱动抢同一个名字是配置错误，让进程起不来比让线上随机走到
	// 其中一个要好。
	Register(d Driver) error
	// MustRegister 是 Register 的 panic 版本，供 cmd 装配期使用——
	// 那时进程还没开始服务，panic 比返回错误更早暴露问题。
	MustRegister(d Driver)
}

func (r *registry) Register(d Driver) error {
	if d == nil {
		return fmt.Errorf("adapter: 不能注册 nil 驱动")
	}
	name := d.Name()
	if name == "" {
		return fmt.Errorf("adapter: 驱动 %T 的 Name() 为空", d)
	}

	sd, isSync := d.(SyncDriver)
	ad, isAsync := d.(AsyncDriver)
	if !isSync && !isAsync {
		return fmt.Errorf("adapter: 驱动 %q(%T) 既不是 SyncDriver 也不是 AsyncDriver", name, d)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if isSync {
		if _, dup := r.sync[name]; dup {
			return fmt.Errorf("adapter: 同步驱动 %q 重复注册", name)
		}
	}
	if isAsync {
		if _, dup := r.async[name]; dup {
			return fmt.Errorf("adapter: 异步驱动 %q 重复注册", name)
		}
	}
	if isSync {
		r.sync[name] = sd
	}
	if isAsync {
		r.async[name] = ad
	}
	return nil
}

func (r *registry) MustRegister(d Driver) {
	if err := r.Register(d); err != nil {
		panic(err)
	}
}

func (r *registry) Sync(name string) (SyncDriver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.sync[name]
	return d, ok
}

func (r *registry) Async(name string) (AsyncDriver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.async[name]
	return d, ok
}

func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]struct{}, len(r.sync)+len(r.async))
	for name := range r.sync {
		seen[name] = struct{}{}
	}
	for name := range r.async {
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	// 排序让 admin 的可选协议列表稳定。map 迭代顺序随机会让同一份配置
	// 在两次刷新之间抖动，看着像配置变了。
	sort.Strings(out)
	return out
}

// DriverName 是**全系统唯一**一处把模型配置翻译成驱动查找键的地方。
//
// video 族按 VideoProtocol 分驱动，其余族按 ProtocolFamily 本身查找。
// 这个 if 必须只存在于这里：一旦调用方各自拼这个键，
// "video 内部还要再分一层" 这条知识就散到了全栈。
func DriverName(m domain.ModelConfig) string {
	if m.Family == domain.FamilyVideo && m.VideoProtocol != nil {
		return string(*m.VideoProtocol)
	}
	return string(m.Family)
}

// ResolveSync 按模型配置取同步驱动。
func ResolveSync(r Registry, m domain.ModelConfig) (SyncDriver, error) {
	name := DriverName(m)
	d, ok := r.Sync(name)
	if !ok {
		return nil, fmt.Errorf("adapter: 模型 %s 需要同步驱动 %q，但它未注册", m.ID, name)
	}
	return d, nil
}

// ResolveAsync 按模型配置取异步驱动。
func ResolveAsync(r Registry, m domain.ModelConfig) (AsyncDriver, error) {
	name := DriverName(m)
	d, ok := r.Async(name)
	if !ok {
		return nil, fmt.Errorf("adapter: 模型 %s 需要异步驱动 %q，但它未注册", m.ID, name)
	}
	return d, nil
}

// IsAsync 判定某个模型该走异步驱动还是同步驱动。
//
// 不能简单地"查得到异步就走异步"：**同一个名字可以同时挂着同步与异步两个
// 驱动**，mock 就是这样——一个出图（protocol_family='mock'），一个出视频
// （protocol_family='video' + video_protocol='mock'），两者共用查找键 "mock"。
// 无脑先查异步会把图像任务送进那个产视频的驱动。
//
// 判据是驱动自报的 Family 与模型配置的 protocol_family 对不对得上。
// 这仍然不需要认识任何具体供应商，也不假定"异步就等于视频"。
func IsAsync(r Registry, m domain.ModelConfig) bool {
	if r == nil {
		return false
	}
	name := DriverName(m)
	async, hasAsync := r.Async(name)
	if !hasAsync {
		return false
	}
	if _, hasSync := r.Sync(name); !hasSync {
		return true
	}
	return async.Family() == m.Family
}
