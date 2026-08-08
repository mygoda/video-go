package config

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateHTTPAddrRejectsLowPorts 钉住本地端口规范里最要紧的一条：
// 8080 及一切低位端口必须在启动时就被拒绝。这条规矩来自一次 5.5 小时的
// 静默中断（详见 MinPort 的注释），退化成「文档里写了但代码不管」就等于没有。
func TestValidateHTTPAddrRejectsLowPorts(t *testing.T) {
	for _, addr := range []string{":8080", "127.0.0.1:8080", ":80", ":9999", "localhost:5173"} {
		err := validateHTTPAddr(addr)
		if err == nil {
			t.Errorf("validateHTTPAddr(%q) accepted a port below %d", addr, MinPort)
			continue
		}
		if !errors.Is(err, ErrPortPolicy) {
			t.Errorf("validateHTTPAddr(%q) error is not ErrPortPolicy: %v", addr, err)
		}
		// 报错要能直接照着改，所以必须把犯规的地址本身带出来。
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("validateHTTPAddr(%q) error does not name the address: %v", addr, err)
		}
	}
}

// TestValidateHTTPAddrRequiresExplicitPort 端口留空会让内核随机分配，
// 进程实际听在哪没人知道，前端代理也就无从配起——这正是"静默失败"的另一种形态。
func TestValidateHTTPAddrRequiresExplicitPort(t *testing.T) {
	for _, addr := range []string{":", "127.0.0.1:", ":0"} {
		if err := validateHTTPAddr(addr); err == nil {
			t.Errorf("validateHTTPAddr(%q) accepted an address without a usable port", addr)
		}
	}
}

func TestValidateHTTPAddrAcceptsCompliantPorts(t *testing.T) {
	for _, addr := range []string{DefaultHTTPAddr, ":10000", "127.0.0.1:18080", "[::1]:18081", ":65535"} {
		if err := validateHTTPAddr(addr); err != nil {
			t.Errorf("validateHTTPAddr(%q) rejected a compliant address: %v", addr, err)
		}
	}
}

// TestValidateHTTPAddrRejectsMalformed 地址根本不成形时报的是格式错误，
// 而不是被 SplitHostPort 的失败悄悄放行。
func TestValidateHTTPAddrRejectsMalformed(t *testing.T) {
	for _, addr := range []string{"18080", "", "not-an-addr", ":abc"} {
		if err := validateHTTPAddr(addr); err == nil {
			t.Errorf("validateHTTPAddr(%q) accepted a malformed listen address", addr)
		}
	}
}

// TestDefaultsObeyPortPolicy 仓库自带的缺省值本身必须合规——
// 本 issue 的起因就是缺省值年久失修地停在 8080。
func TestDefaultsObeyPortPolicy(t *testing.T) {
	if err := validateHTTPAddr(DefaultHTTPAddr); err != nil {
		t.Errorf("DefaultHTTPAddr %q violates the port policy: %v", DefaultHTTPAddr, err)
	}
	if strings.Contains(DefaultPublicBaseURL, ":8080") {
		t.Errorf("DefaultPublicBaseURL still points at 8080: %q", DefaultPublicBaseURL)
	}
	if !strings.Contains(DefaultPublicBaseURL, strings.TrimPrefix(DefaultHTTPAddr, ":")) {
		t.Errorf("DefaultPublicBaseURL %q does not match DefaultHTTPAddr %q",
			DefaultPublicBaseURL, DefaultHTTPAddr)
	}
}
