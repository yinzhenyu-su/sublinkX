package node

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// 本文件负责把「上游订阅载荷」转换成分享链接列表。
//
// 支持的三种形态（实测于 x-ui 的订阅地址）：
//  1. 单个 Xray 配置对象   —— 单客户端 subId（/jjsson/<subId>）
//  2. Xray 配置对象数组     —— 多客户端 subId
//  3. base64 编码的链接列表 —— /ssuubb/<subId>（无客户端 UA 时）或其它面板的默认格式
//
// 生成的链接会被现有的 EncodeClash / EncodeSurge / Decode*URL 消费，
// 因此这里只需要保证「链接本身合法且字段完整」。

// ---------- Xray 配置的最小字段集 ----------

type xrayStream struct {
	Network         string                 `json:"network"`
	Security        string                 `json:"security"`
	TLSSettings     map[string]interface{} `json:"tlsSettings"`
	RealitySettings map[string]interface{} `json:"realitySettings"`
	WSSettings      map[string]interface{} `json:"wsSettings"`
	GRPCSettings    map[string]interface{} `json:"grpcSettings"`
	XHTTPSettings   map[string]interface{} `json:"xhttpSettings"`
	TCPSettings     map[string]interface{} `json:"tcpSettings"`
}

type xrayOutbound struct {
	Protocol       string                 `json:"protocol"`
	Tag            string                 `json:"tag"`
	Settings       map[string]interface{} `json:"settings"`
	StreamSettings xrayStream             `json:"streamSettings"`
}

type xrayConfig struct {
	Remarks   string         `json:"remarks"`
	Outbounds []xrayOutbound `json:"outbounds"`
}

// 不可作为节点的 outbound tag
var ignoredOutboundTags = map[string]bool{
	"direct": true, "blocked": true, "block": true, "dns_out": true, "dns": true,
}

// DecodeSubPayload 把上游订阅载荷解析成分享链接列表。
// 先按 Xray JSON 解析，失败再按 base64 链接列表兜底；两者都不行才返回错误。
func DecodeSubPayload(raw []byte) ([]string, error) {
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return nil, errors.New("订阅内容为空")
	}
	if links, err := decodeXrayJSON(body); err == nil && len(links) > 0 {
		return links, nil
	}
	links := decodeBase64Links(body)
	if len(links) == 0 {
		return nil, errors.New("无法解析订阅内容（既不是 Xray JSON，也不是 base64 链接列表）")
	}
	return links, nil
}

// decodeBase64Links 解析 base64 形态（/ssuubb/ 那种「一行一个链接」）
func decodeBase64Links(body string) []string {
	decoded := Base64Decode(body)
	var links []string
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "://") {
			links = append(links, line)
		}
	}
	return links
}

// decodeXrayJSON 解析 Xray 配置（对象或数组）
func decodeXrayJSON(body string) ([]string, error) {
	var list []xrayConfig
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) == 0 {
		var single xrayConfig
		if err2 := json.Unmarshal([]byte(body), &single); err2 != nil || len(single.Outbounds) == 0 {
			return nil, errors.New("不是有效的 Xray 配置")
		}
		list = []xrayConfig{single}
	}

	var links []string
	for _, cfg := range list {
		ob, ok := pickProxyOutbound(cfg.Outbounds)
		if !ok {
			log.Println("[订阅源] 配置里没有可识别的节点，跳过。remarks =", cfg.Remarks)
			continue
		}
		link, err := outboundToLink(cfg, ob)
		if err != nil {
			// 单个节点不支持不能拖垮整个源
			log.Println("[订阅源] 跳过不支持的节点:", cfg.Remarks, err)
			continue
		}
		links = append(links, link)
	}
	if len(links) == 0 {
		return nil, errors.New("配置里没有可转换的节点")
	}
	return links, nil
}

// pickProxyOutbound 选真正的节点 outbound：优先 tag=proxy，否则第一个协议可识别的
func pickProxyOutbound(obs []xrayOutbound) (xrayOutbound, bool) {
	for _, ob := range obs {
		if strings.EqualFold(ob.Tag, "proxy") && isSupportedProtocol(ob.Protocol) {
			return ob, true
		}
	}
	for _, ob := range obs {
		if ignoredOutboundTags[strings.ToLower(ob.Tag)] {
			continue
		}
		if isSupportedProtocol(ob.Protocol) {
			return ob, true
		}
	}
	return xrayOutbound{}, false
}

func isSupportedProtocol(p string) bool {
	switch strings.ToLower(p) {
	case "vless", "vmess", "trojan", "ss", "shadowsocks":
		return true
	}
	return false
}

// outboundToLink 把「配置 + 节点 outbound」转成分享链接
func outboundToLink(cfg xrayConfig, ob xrayOutbound) (string, error) {
	switch strings.ToLower(ob.Protocol) {
	case "vless":
		return buildVLESSLink(cfg, ob)
	case "vmess":
		return buildVMessLink(cfg, ob)
	case "trojan":
		return buildTrojanLink(cfg, ob)
	case "ss", "shadowsocks":
		return buildSSLink(cfg, ob)
	}
	return "", fmt.Errorf("暂不支持的协议: %s", ob.Protocol)
}

// ---------- 链接构造 ----------

func buildVLESSLink(cfg xrayConfig, ob xrayOutbound) (string, error) {
	addr, port, id, flow := "", 0, "", ""
	if v := str(ob.Settings["address"]); v != "" { // x-ui 的扁平形态
		addr, port = v, intVal(ob.Settings["port"])
		id, flow = str(ob.Settings["id"]), str(ob.Settings["flow"])
	} else { // 标准 Xray 的 vnext 形态
		a, p, user := vnextEndpoint(ob.Settings)
		addr, port = a, p
		id, flow = str(user["id"]), str(user["flow"])
	}
	if addr == "" || port == 0 || id == "" {
		return "", errors.New("vless 节点缺少 address/port/id")
	}

	q := url.Values{}
	q.Set("encryption", firstNonEmpty(str(ob.Settings["encryption"]), "none"))
	if flow != "" {
		q.Set("flow", flow)
	}
	if err := transportParams(ob.StreamSettings, q); err != nil {
		return "", err
	}
	if err := securityParams(ob.StreamSettings, q); err != nil {
		return "", err
	}
	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", id, addr, port, q.Encode(), linkName(cfg.Remarks, addr, port)), nil
}

func buildVMessLink(cfg xrayConfig, ob xrayOutbound) (string, error) {
	addr, port := "", 0
	id, aid, scy := "", "0", "auto"
	if v := str(ob.Settings["address"]); v != "" {
		addr, port = v, intVal(ob.Settings["port"])
		id, aid = str(ob.Settings["id"]), firstNonEmpty(str(ob.Settings["aid"]), "0")
		scy = firstNonEmpty(str(ob.Settings["scy"]), str(ob.Settings["security"]), "auto")
	} else {
		a, p, user := vnextEndpoint(ob.Settings)
		addr, port = a, p
		id = str(user["id"])
		if a := str(user["alterId"]); a != "" {
			aid = a
		}
		scy = firstNonEmpty(str(user["security"]), "auto")
	}
	if addr == "" || port == 0 || id == "" {
		return "", errors.New("vmess 节点缺少 address/port/id")
	}

	// vmess 的传输层信息走 JSON 字段（net/type/host/path/tls/sni/alpn/fp）
	net, headerType, host, path, mode := "tcp", "none", "", "", ""
	ss := ob.StreamSettings
	switch strings.ToLower(ss.Network) {
	case "", "tcp":
		net = "tcp"
		if t := ss.TCPSettings; t != nil {
			if h := m(t["header"]); h != nil {
				if ht := str(h["type"]); ht != "" {
					headerType = ht
				}
			}
		}
	case "ws", "websocket":
		net, path = "ws", ""
		if ws := ss.WSSettings; ws != nil {
			path = str(ws["path"])
			host = str(ws["host"])
			if h := m(ws["headers"]); h != nil {
				if v := str(h["Host"]); v != "" {
					host = v
				}
			}
		}
	case "grpc":
		net = "grpc"
		if g := ss.GRPCSettings; g != nil {
			path = str(g["serviceName"]) // vmess 链接里 grpc 的 service name 走 path
			if boolVal(g["multiMode"]) {
				mode = "multi"
			}
		}
	case "xhttp", "splithttp":
		net = "xhttp"
		if x := ss.XHTTPSettings; x != nil {
			path = str(x["path"])
			host = str(x["host"])
			mode = str(x["mode"])
		}
	default:
		return "", fmt.Errorf("vmess 暂不支持的传输: %s", ss.Network)
	}

	tls := ""
	sni, alpn, fp := "", "", ""
	switch strings.ToLower(ss.Security) {
	case "", "none":
	case "tls":
		tls = "tls"
		if t := ss.TLSSettings; t != nil {
			sni = str(t["serverName"])
			alpn = strings.Join(strList(t["alpn"]), ",")
			fp = str(t["fingerprint"])
		}
	default:
		return "", fmt.Errorf("vmess 暂不支持的安全类型: %s", ss.Security)
	}

	obj := map[string]interface{}{
		"v": "2", "ps": nameOrEmpty(cfg.Remarks, addr, port),
		"add": addr, "port": strconv.Itoa(port),
		"id": id, "aid": aid, "scy": scy,
		"net": net, "type": headerType,
		"host": host, "path": path,
		"tls": tls, "sni": sni, "alpn": alpn, "fp": fp,
	}
	if mode != "" {
		obj["mode"] = mode
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(b), nil
}

func buildTrojanLink(cfg xrayConfig, ob xrayOutbound) (string, error) {
	addr, port, password := "", 0, ""
	if v := str(ob.Settings["address"]); v != "" {
		addr, port, password = v, intVal(ob.Settings["port"]), str(ob.Settings["password"])
	} else {
		a, p, server := serversEndpoint(ob.Settings)
		addr, port = a, p
		password = str(server["password"])
	}
	if addr == "" || port == 0 || password == "" {
		return "", errors.New("trojan 节点缺少 address/port/password")
	}

	q := url.Values{}
	if err := transportParams(ob.StreamSettings, q); err != nil {
		return "", err
	}
	if err := securityParams(ob.StreamSettings, q); err != nil {
		return "", err
	}
	return fmt.Sprintf("trojan://%s@%s:%d?%s#%s", url.QueryEscape(password), addr, port, q.Encode(),
		linkName(cfg.Remarks, addr, port)), nil
}

func buildSSLink(cfg xrayConfig, ob xrayOutbound) (string, error) {
	addr, port := "", 0
	method, password := "", ""
	if v := str(ob.Settings["address"]); v != "" {
		addr, port = v, intVal(ob.Settings["port"])
		method, password = str(ob.Settings["method"]), str(ob.Settings["password"])
	} else {
		a, p, server := serversEndpoint(ob.Settings)
		addr, port = a, p
		method, password = str(server["method"]), str(server["password"])
	}
	if addr == "" || port == 0 || method == "" {
		return "", errors.New("ss 节点缺少 address/port/method")
	}
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
	return fmt.Sprintf("ss://%s@%s:%d#%s", userinfo, addr, port, linkName(cfg.Remarks, addr, port)), nil
}

// ---------- 传输层 / 安全层参数 ----------

// transportParams 把 streamSettings 的传输层信息写进查询参数
func transportParams(ss xrayStream, q url.Values) error {
	switch strings.ToLower(ss.Network) {
	case "", "tcp":
		q.Set("type", "tcp")
		if t := ss.TCPSettings; t != nil {
			if h := m(t["header"]); h != nil {
				if ht := str(h["type"]); ht != "" && ht != "none" {
					q.Set("headerType", ht)
				}
			}
		}
	case "ws", "websocket":
		q.Set("type", "ws")
		if ws := ss.WSSettings; ws != nil {
			if p := str(ws["path"]); p != "" {
				q.Set("path", p)
			}
			host := str(ws["host"])
			if h := m(ws["headers"]); h != nil {
				if v := str(h["Host"]); v != "" {
					host = v
				}
			}
			if host != "" {
				q.Set("host", host)
			}
		}
	case "grpc":
		q.Set("type", "grpc")
		if g := ss.GRPCSettings; g != nil {
			if sn := str(g["serviceName"]); sn != "" {
				q.Set("serviceName", sn)
			}
			if boolVal(g["multiMode"]) {
				q.Set("mode", "multi")
			}
		}
	case "xhttp", "splithttp":
		q.Set("type", "xhttp")
		if x := ss.XHTTPSettings; x != nil {
			if p := str(x["path"]); p != "" {
				q.Set("path", p)
			}
			if h := str(x["host"]); h != "" {
				q.Set("host", h)
			}
			if md := str(x["mode"]); md != "" {
				q.Set("mode", md)
			}
		}
	default:
		// h2/http/quic 等已被 Xray 移除，明确报错而不是生成连不上的链接
		return fmt.Errorf("暂不支持的传输: %s", ss.Network)
	}
	return nil
}

// securityParams 把 streamSettings 的安全层信息写进查询参数
func securityParams(ss xrayStream, q url.Values) error {
	switch strings.ToLower(ss.Security) {
	case "", "none":
		// 明文，不写 security 参数
	case "tls":
		q.Set("security", "tls")
		if t := ss.TLSSettings; t != nil {
			if sni := str(t["serverName"]); sni != "" {
				q.Set("sni", sni)
			}
			if alpn := strList(t["alpn"]); len(alpn) > 0 {
				q.Set("alpn", strings.Join(alpn, ","))
			}
			if fp := str(t["fingerprint"]); fp != "" {
				q.Set("fp", fp)
			}
			if boolVal(t["allowInsecure"]) {
				q.Set("allowInsecure", "1")
			}
		}
	case "reality":
		q.Set("security", "reality")
		if r := ss.RealitySettings; r != nil {
			if sni := str(r["serverName"]); sni != "" {
				q.Set("sni", sni)
			}
			if fp := str(r["fingerprint"]); fp != "" {
				q.Set("fp", fp)
			}
			if pbk := str(r["publicKey"]); pbk != "" {
				q.Set("pbk", pbk)
			}
			if sid := str(r["shortId"]); sid != "" {
				q.Set("sid", sid)
			}
			if spx := str(r["spiderX"]); spx != "" {
				q.Set("spx", spx)
			}
		}
	default:
		return fmt.Errorf("暂不支持的安全类型: %s", ss.Security)
	}
	return nil
}

// ---------- 小工具 ----------

// vnextEndpoint 标准 Xray 的 vnext 形态
func vnextEndpoint(settings map[string]interface{}) (string, int, map[string]interface{}) {
	for _, v := range arr(settings["vnext"]) {
		mv := m(v)
		if mv == nil {
			continue
		}
		var user map[string]interface{}
		if us := arr(mv["users"]); len(us) > 0 {
			user = m(us[0])
		}
		return str(mv["address"]), intVal(mv["port"]), user
	}
	return "", 0, nil
}

// serversEndpoint 标准 Xray 的 servers 形态（trojan / ss）
func serversEndpoint(settings map[string]interface{}) (string, int, map[string]interface{}) {
	for _, v := range arr(settings["servers"]) {
		mv := m(v)
		if mv == nil {
			continue
		}
		return str(mv["address"]), intVal(mv["port"]), mv
	}
	return "", 0, nil
}

func nameOrEmpty(remarks, addr string, port int) string {
	if strings.TrimSpace(remarks) != "" {
		return remarks
	}
	return fmt.Sprintf("%s:%d", addr, port)
}

// linkName 生成链接的 #fragment（节点名）
func linkName(remarks, addr string, port int) string {
	return url.QueryEscape(nameOrEmpty(remarks, addr, port))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func str(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

func intVal(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

func boolVal(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	case float64:
		return x != 0
	}
	return false
}

func strList(v interface{}) []string {
	items := arr(v)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s := str(it); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func m(v interface{}) map[string]interface{} {
	if mm, ok := v.(map[string]interface{}); ok {
		return mm
	}
	return nil
}

func arr(v interface{}) []interface{} {
	if aa, ok := v.([]interface{}); ok {
		return aa
	}
	return nil
}
