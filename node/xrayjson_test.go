package node

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// 真实 fixture（由线上 x-ui 订阅抓取而来）：
//   - xray_ws_multi.json    : 多客户端 subId → 配置对象数组（2 个 ws+tls 节点）
//   - xray_reality_single.json : 单客户端 subId → 单个配置对象（reality+tcp+vision）
func TestDecodeSubPayloadRealFixtures(t *testing.T) {
	wsRaw, err := os.ReadFile("testdata/xray_ws_multi.json")
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	links, err := DecodeSubPayload(wsRaw)
	if err != nil {
		t.Fatalf("解析 ws 多节点订阅失败: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("期望 2 条链接，实际 %d: %v", len(links), links)
	}
	// 两个节点各自的预期（fixture 实测：第二个节点没设 ws host）
	type expect struct{ host, fp, path string }
	expects := map[string]expect{
		"node-ws-with-host":    {host: "node.example.com", fp: "safari", path: "/ws"},
		"node-ws-without-host": {host: "", fp: "chrome", path: "/ws"},
	}
	for _, link := range links {
		if !strings.HasPrefix(link, "vless://") {
			t.Errorf("期望 vless 链接，实际: %s", link)
		}
		v, err := DecodeVLESSURL(link)
		if err != nil {
			t.Fatalf("生成的链接无法被 DecodeVLESSURL 解析: %v (%s)", err, link)
		}
		if v.Query.Type != "ws" {
			t.Errorf("network 期望 ws，实际 %s", v.Query.Type)
		}
		if v.Query.Security != "tls" {
			t.Errorf("security 期望 tls，实际 %s", v.Query.Security)
		}
		name := link[strings.LastIndex(link, "#")+1:]
		e, ok := expects[name]
		if !ok {
			t.Errorf("出现未预期的节点名: %s", name)
			continue
		}
		if v.Query.Path != e.path {
			t.Errorf("%s path 期望 %s，实际 %s", name, e.path, v.Query.Path)
		}
		if v.Query.Host != e.host {
			t.Errorf("%s host 期望 %q，实际 %q", name, e.host, v.Query.Host)
		}
		if v.Query.Sni != "node.example.com" || v.Query.Fp != e.fp {
			t.Errorf("%s sni/fp 不对: sni=%s fp=%s", name, v.Query.Sni, v.Query.Fp)
		}
		if len(v.Query.Alpn) == 0 {
			t.Errorf("%s alpn 不应该为空", name)
		}
	}
	if !strings.HasSuffix(links[0], "#node-ws-with-host") {
		t.Errorf("节点名（remarks）没有写进 fragment: %s", links[0])
	}

	// reality 单对象形态
	reRaw, err := os.ReadFile("testdata/xray_reality_single.json")
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	links, err = DecodeSubPayload(reRaw)
	if err != nil {
		t.Fatalf("解析 reality 订阅失败: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("期望 1 条链接，实际 %d", len(links))
	}
	v, err := DecodeVLESSURL(links[0])
	if err != nil {
		t.Fatalf("reality 链接无法解析: %v", err)
	}
	if v.Query.Security != "reality" {
		t.Errorf("security 期望 reality，实际 %s", v.Query.Security)
	}
	if v.Query.Pbk == "" || v.Query.Sid == "" {
		t.Errorf("reality 关键参数缺失: pbk=%s sid=%s", v.Query.Pbk, v.Query.Sid)
	}
	if !strings.Contains(links[0], "spx=") {
		t.Errorf("reality 链接应带 spiderX: %s", links[0])
	}
	if v.Query.Flow != "xtls-rprx-vision" {
		t.Errorf("flow 期望 xtls-rprx-vision，实际 %s", v.Query.Flow)
	}
	if v.Query.Fp != "chrome" {
		t.Errorf("fingerprint 期望 chrome，实际 %s", v.Query.Fp)
	}
	if !strings.HasSuffix(links[0], "#node-reality-vision") {
		t.Errorf("reality 节点名不对: %s", links[0])
	}
}

// 各传输层 / 安全类型
func TestDecodeSubPayloadTransports(t *testing.T) {
	cases := []struct {
		desc     string
		raw      string
		wantType string
		wantSec  string
		check    func(t *testing.T, q map[string]string)
	}{
		{
			desc: "xhttp",
			raw: `{"remarks":"xhttp 节点","outbounds":[{"protocol":"vless","tag":"proxy",
			  "settings":{"address":"a.com","port":8444,"id":"11111111-1111-1111-1111-111111111111","encryption":"none"},
			  "streamSettings":{"network":"xhttp","security":"tls",
			    "tlsSettings":{"serverName":"a.com","alpn":["h2"]},
			    "xhttpSettings":{"path":"/xhp","host":"a.com","mode":"stream-one"}}}]}`,
			wantType: "xhttp", wantSec: "tls",
			check: func(t *testing.T, q map[string]string) {
				if q["path"] != "/xhp" || q["host"] != "a.com" || q["mode"] != "stream-one" {
					t.Errorf("xhttp 参数不对: %v", q)
				}
			},
		},
		{
			desc: "grpc",
			raw: `{"remarks":"grpc 节点","outbounds":[{"protocol":"vless","tag":"proxy",
			  "settings":{"vnext":[{"address":"b.com","port":443,"users":[{"id":"22222222-2222-2222-2222-222222222222"}]}]},
			  "streamSettings":{"network":"grpc","security":"tls",
			    "tlsSettings":{"serverName":"b.com"},
			    "grpcSettings":{"serviceName":"gsvc","multiMode":true}}}]}`,
			wantType: "grpc", wantSec: "tls",
			check: func(t *testing.T, q map[string]string) {
				if q["serviceName"] != "gsvc" || q["mode"] != "multi" {
					t.Errorf("grpc 参数不对: %v", q)
				}
			},
		},
		{
			desc: "tcp 明文（无 security）",
			raw: `{"remarks":"裸 tcp","outbounds":[{"protocol":"vless","tag":"proxy",
			  "settings":{"address":"c.com","port":8080,"id":"33333333-3333-3333-3333-333333333333"},
			  "streamSettings":{"network":"tcp"}}]}`,
			wantType: "tcp", wantSec: "",
			check: func(t *testing.T, q map[string]string) {
				if q["encryption"] != "none" {
					t.Errorf("encryption 默认应为 none: %v", q)
				}
			},
		},
		{
			desc: "未知传输应被跳过（h2 已被 Xray 移除）",
			raw: `{"remarks":"h2 节点","outbounds":[{"protocol":"vless","tag":"proxy",
			  "settings":{"address":"d.com","port":443,"id":"44444444-4444-4444-4444-444444444444"},
			  "streamSettings":{"network":"h2","security":"tls"}}]}`,
			wantType: "", wantSec: "",
		},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			links, err := DecodeSubPayload([]byte(c.raw))
			if c.wantType == "" {
				if err == nil {
					t.Fatalf("期望解析失败（跳过不支持节点），实际得到 %v", links)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(links) != 1 {
				t.Fatalf("期望 1 条链接，实际 %d", len(links))
			}
			v, err := DecodeVLESSURL(links[0])
			if err != nil {
				t.Fatalf("链接无法解析: %v", err)
			}
			if v.Query.Type != c.wantType {
				t.Errorf("type 期望 %s，实际 %s", c.wantType, v.Query.Type)
			}
			if v.Query.Security != c.wantSec {
				t.Errorf("security 期望 %q，实际 %q", c.wantSec, v.Query.Security)
			}
			if c.check != nil {
				c.check(t, map[string]string{
					"path": v.Query.Path, "host": v.Query.Host, "mode": v.Query.Mode,
					"serviceName": v.Query.ServiceName, "encryption": v.Query.Encryption,
				})
			}
		})
	}
}

// vmess / trojan / ss
func TestDecodeSubPayloadOtherProtocols(t *testing.T) {
	t.Run("vmess+xhttp", func(t *testing.T) {
		raw := `{"remarks":"vm-xhttp","outbounds":[{"protocol":"vmess","tag":"proxy",
		  "settings":{"vnext":[{"address":"e.com","port":8444,"users":[{"id":"55555555-5555-5555-5555-555555555555","alterId":0,"security":"auto"}]}]},
		  "streamSettings":{"network":"xhttp","security":"tls","tlsSettings":{"serverName":"e.com"},
		    "xhttpSettings":{"path":"/vm","host":"e.com","mode":"auto"}}}]}`
		links, err := DecodeSubPayload([]byte(raw))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !strings.HasPrefix(links[0], "vmess://") {
			t.Fatalf("期望 vmess 链接: %s", links[0])
		}
		vm, err := DecodeVMESSURL(links[0])
		if err != nil {
			t.Fatalf("vmess 链接无法解析: %v", err)
		}
		if vm.Net != "xhttp" || vm.Path != "/vm" || vm.Host != "e.com" || vm.Sni != "e.com" {
			t.Errorf("vmess 字段不对: net=%s path=%s host=%s sni=%s", vm.Net, vm.Path, vm.Host, vm.Sni)
		}
	})

	t.Run("trojan+ws", func(t *testing.T) {
		raw := `{"remarks":"tj","outbounds":[{"protocol":"trojan","tag":"proxy",
		  "settings":{"servers":[{"address":"f.com","port":443,"password":"pw123"}]},
		  "streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"f.com"},
		    "wsSettings":{"path":"/tj","headers":{"Host":"f.com"}}}}]}`
		links, err := DecodeSubPayload([]byte(raw))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		tj, err := DecodeTrojanURL(links[0])
		if err != nil {
			t.Fatalf("trojan 链接无法解析: %v", err)
		}
		if tj.Password != "pw123" || tj.Query.Type != "ws" || tj.Query.Path != "/tj" {
			t.Errorf("trojan 字段不对: %+v", tj)
		}
	})

	t.Run("ss", func(t *testing.T) {
		raw := `{"remarks":"ss 节点","outbounds":[{"protocol":"shadowsocks","tag":"proxy",
		  "settings":{"servers":[{"address":"g.com","port":8388,"method":"aes-256-gcm","password":"sspw"}]}}]}`
		links, err := DecodeSubPayload([]byte(raw))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !strings.HasPrefix(links[0], "ss://") {
			t.Fatalf("期望 ss 链接: %s", links[0])
		}
		ss, err := DecodeSSURL(links[0])
		if err != nil {
			t.Fatalf("ss 链接无法解析: %v", err)
		}
		if ss.Param.Cipher != "aes-256-gcm" || ss.Param.Password != "sspw" {
			t.Errorf("ss 字段不对: %+v", ss)
		}
	})

	t.Run("direct/blocked 被忽略", func(t *testing.T) {
		raw := `{"remarks":"带干扰项","outbounds":[
		  {"protocol":"freedom","tag":"direct"},
		  {"protocol":"blackhole","tag":"blocked"},
		  {"protocol":"vless","tag":"proxy","settings":{"address":"h.com","port":443,"id":"66666666-6666-6666-6666-666666666666"},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"h.com"}}}]}`
		links, err := DecodeSubPayload([]byte(raw))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(links) != 1 {
			t.Fatalf("期望只取 proxy 一个节点，实际 %d", len(links))
		}
	})
}

// base64 形态（/ssuubb/ 无客户端 UA 时）与错误分支
func TestDecodeSubPayloadBase64AndErrors(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte(
		"vless://11111111-1111-1111-1111-111111111111@node.example.com:443?type=tcp&security=tls&sni=node.example.com#n1\n" +
			"vmess://eyJ2IjoiMiJ9\n"))
	links, err := DecodeSubPayload([]byte(raw))
	if err != nil {
		t.Fatalf("base64 形态解析失败: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("期望 2 条链接，实际 %d: %v", len(links), links)
	}

	for _, bad := range []string{"", "   ", "这不是订阅也不是 base64 链接", "<html><body>订阅页面</body></html>"} {
		if _, err := DecodeSubPayload([]byte(bad)); err == nil {
			t.Errorf("期望解析失败，输入: %q", bad)
		}
	}
}

// 生成的链接必须能被现有 EncodeClash 正确转成 clash 节点（端到端串联）
func TestDecodeSubPayloadThenEncodeClash(t *testing.T) {
	raw, err := os.ReadFile("testdata/xray_ws_multi.json")
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	links, err := DecodeSubPayload(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	p := encodeOne(t, links[0])
	if p["network"] != "ws" {
		t.Errorf("clash network 期望 ws，实际 %v", p["network"])
	}
	if _, ok := p["ws-opts"]; !ok {
		t.Errorf("clash 节点缺少 ws-opts: %v", keys(p))
	}
}
