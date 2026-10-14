package models

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// setupTestDB 每个用例一个独立的临时 sqlite 库
func setupTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&User{}, &Subcription{}, &SubLogs{}, &GroupNode{}, &Node{}, &SubSource{}); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	DB = db
	SubSourceEnabled = true
	sourceLocks = sync.Map{}
}

func newTestSub(t *testing.T, name string) Subcription {
	t.Helper()
	sub := Subcription{Name: name, Config: `{"clash":"./template/clash.yaml"}`}
	if err := DB.Create(&sub).Error; err != nil {
		t.Fatalf("创建订阅失败: %v", err)
	}
	return sub
}

func nodesOfSource(t *testing.T, id int) []Node {
	t.Helper()
	var ns []Node
	if err := DB.Where("source_id = ?", id).Find(&ns).Error; err != nil {
		t.Fatalf("查询节点失败: %v", err)
	}
	return ns
}

// 对账：新增 / 幂等 / 改名 / Prune / 不误伤手动节点
func TestSyncNodesReconcile(t *testing.T) {
	setupTestDB(t)
	sub := newTestSub(t, "t-sub")
	s := SubSource{Name: "src", URL: "http://example.invalid/sub", SubcriptionID: sub.ID, Group: "my", Enabled: true, Interval: 600}
	if err := DB.Create(&s).Error; err != nil {
		t.Fatalf("创建源失败: %v", err)
	}

	// 手动节点（不属于任何源），不应该被同步逻辑碰到
	manual := Node{Name: "manual-node", Link: "vless://manual@z.com:443?type=tcp#manual-node"}
	if err := DB.Create(&manual).Error; err != nil {
		t.Fatalf("创建手动节点失败: %v", err)
	}
	if err := DB.Model(&sub).Association("Nodes").Append(&manual); err != nil {
		t.Fatalf("手动节点入订阅失败: %v", err)
	}
	if err := DB.Model(&sub).Update("node_order", "manual-node").Error; err != nil {
		t.Fatalf("设置 NodeOrder 失败: %v", err)
	}

	l1 := "vless://u1@a.com:443?type=tcp&security=tls&sni=a.com#node-1"
	l2 := "vless://u2@b.com:443?type=ws&security=tls&sni=b.com&path=%2Fws#node-2"

	// 1) 新增
	if err := s.syncNodes([]string{l1, l2}); err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	ns := nodesOfSource(t, s.ID)
	if len(ns) != 2 {
		t.Fatalf("期望 2 个源节点，实际 %d", len(ns))
	}

	// 分组绑定
	var gn GroupNode
	if err := DB.Preload("Nodes").Where("name = ?", "my").First(&gn).Error; err != nil {
		t.Fatalf("分组 my 不存在: %v", err)
	}
	if len(gn.Nodes) != 2 {
		t.Errorf("分组 my 应绑定 2 个节点，实际 %d", len(gn.Nodes))
	}

	// 订阅里应该有 手动 + 2 个源节点，且 NodeOrder 同步维护（Find 会按 NodeOrder 过滤）
	var loaded Subcription
	if err := DB.Preload("Nodes").Where("id = ?", sub.ID).First(&loaded).Error; err != nil {
		t.Fatalf("读取订阅失败: %v", err)
	}
	if len(loaded.Nodes) != 3 {
		t.Errorf("订阅应有 3 个节点，实际 %d", len(loaded.Nodes))
	}
	for _, name := range []string{"manual-node", "node-1", "node-2"} {
		if !strings.Contains(loaded.NodeOrder, name) {
			t.Errorf("NodeOrder 缺少 %s: %s", name, loaded.NodeOrder)
		}
	}

	// 2) 幂等：同样内容再同步一次，数量不变
	if err := s.syncNodes([]string{l1, l2}); err != nil {
		t.Fatalf("二次同步失败: %v", err)
	}
	if got := len(nodesOfSource(t, s.ID)); got != 2 {
		t.Errorf("幂等性失败：期望仍 2 个，实际 %d", got)
	}

	// 3) 改名（同 key 不同 fragment）→ 原地更新，不新增
	renamed := "vless://u1@a.com:443?type=tcp&security=tls&sni=a.com#node-1-renamed"
	if err := s.syncNodes([]string{renamed, l2}); err != nil {
		t.Fatalf("改名同步失败: %v", err)
	}
	ns = nodesOfSource(t, s.ID)
	if len(ns) != 2 {
		t.Errorf("改名后节点数应为 2，实际 %d", len(ns))
	}
	found := false
	for _, n := range ns {
		if n.Name == "node-1-renamed" {
			found = true
		}
	}
	if !found {
		t.Errorf("改名没有生效：%v", ns)
	}

	// 4) Prune=false：源里消失的节点保留
	if err := s.syncNodes([]string{renamed}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if got := len(nodesOfSource(t, s.ID)); got != 2 {
		t.Errorf("Prune=false 时不应删除节点，期望 2 个，实际 %d", got)
	}

	// 5) Prune=true：消失的节点被删除，且从订阅里移除；手动节点不受影响
	s.Prune = true
	if err := s.syncNodes([]string{renamed}); err != nil {
		t.Fatalf("Prune 同步失败: %v", err)
	}
	ns = nodesOfSource(t, s.ID)
	if len(ns) != 1 || ns[0].Name != "node-1-renamed" {
		t.Errorf("Prune=true 后应只剩 node-1-renamed，实际 %v", ns)
	}
	if err := DB.Preload("Nodes").Where("id = ?", sub.ID).First(&loaded).Error; err != nil {
		t.Fatalf("读取订阅失败: %v", err)
	}
	if len(loaded.Nodes) != 2 {
		t.Errorf("订阅应剩 手动 + node-1-renamed 共 2 个，实际 %d", len(loaded.Nodes))
	}
	if strings.Contains(loaded.NodeOrder, "node-2") {
		t.Errorf("NodeOrder 里还残留被删节点: %s", loaded.NodeOrder)
	}
	if !strings.Contains(loaded.NodeOrder, "manual-node") {
		t.Errorf("手动节点被误删: %s", loaded.NodeOrder)
	}
}

// 刷新：间隔门控 / force / 失败退避 / 并发去重 / UA 正确
func TestRefreshGatingAndFailOpen(t *testing.T) {
	setupTestDB(t)

	var hits int64
	var lastUA, lastAccept string
	payload := base64.StdEncoding.EncodeToString([]byte(
		"vless://u1@a.com:443?type=tcp&security=tls&sni=a.com#n1\n"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		lastUA = r.Header.Get("User-Agent")
		lastAccept = r.Header.Get("Accept")
		w.Write([]byte(payload))
	}))
	defer srv.Close()

	sub := newTestSub(t, "t-sub2")
	s := SubSource{Name: "src2", URL: srv.URL, SubcriptionID: sub.ID, Group: "my", Enabled: true, Interval: 600}
	if err := DB.Create(&s).Error; err != nil {
		t.Fatalf("创建源失败: %v", err)
	}

	// 第一次：抓取并入库
	if err := s.Refresh(false); err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("期望抓取 1 次，实际 %d", hits)
	}
	if !s.LastOK {
		t.Errorf("LastOK 应为 true")
	}
	if got := len(nodesOfSource(t, s.ID)); got != 1 {
		t.Fatalf("期望 1 个节点，实际 %d", got)
	}
	if lastUA != DefaultSourceUA {
		t.Errorf("UA 应为 %s，实际 %s", DefaultSourceUA, lastUA)
	}
	if strings.Contains(lastAccept, "text/html") {
		t.Errorf("不应发送 Accept: text/html（上游会返回 HTML 页面），实际 %s", lastAccept)
	}

	// 间隔内：零网络请求
	if err := s.Refresh(false); err != nil {
		t.Fatalf("二次刷新不应报错: %v", err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Errorf("间隔内不应再次抓取，实际抓取 %d 次", hits)
	}

	// force：忽略间隔
	if err := s.Refresh(true); err != nil {
		t.Fatalf("force 刷新失败: %v", err)
	}
	if atomic.LoadInt64(&hits) != 2 {
		t.Errorf("force 应再抓一次，实际 %d", hits)
	}

	// 失败：记录状态 + 推进 LastAt（退避）+ 保留旧节点
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	s.URL = bad.URL
	before := s.LastAt
	if err := s.Refresh(true); err == nil {
		t.Fatalf("上游 500 时应返回错误")
	}
	if s.LastOK {
		t.Errorf("失败后 LastOK 应为 false")
	}
	if !s.LastAt.After(before) {
		t.Errorf("失败也应推进 LastAt（用于退避）")
	}
	if got := len(nodesOfSource(t, s.ID)); got != 1 {
		t.Errorf("失败时旧节点必须保留，实际 %d", got)
	}
	// 间隔内不再重试（快路径直接返回 nil，不把上次的错误再次抛出）
	if err := s.Refresh(false); err != nil {
		t.Errorf("间隔内应走快路径静默返回，实际 %v", err)
	}

	// 返回 HTML（UA 不对的典型情形）→ 视为失败，保留旧节点
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><html><body>Subscription</body></html>"))
	}))
	defer htmlSrv.Close()
	s.URL = htmlSrv.URL
	s.LastAt = time.Time{}
	if err := s.Refresh(true); err == nil {
		t.Errorf("HTML 响应应解析失败")
	}
	if got := len(nodesOfSource(t, s.ID)); got != 1 {
		t.Errorf("解析失败时旧节点必须保留，实际 %d", got)
	}
}

// 并发：同一源同时被多个请求触发，只抓取一次
func TestRefreshConcurrency(t *testing.T) {
	setupTestDB(t)

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		time.Sleep(150 * time.Millisecond) // 拉长窗口，制造并发
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte("vless://u1@a.com:443?type=tcp#c1\n"))))
	}))
	defer srv.Close()

	sub := newTestSub(t, "t-sub3")
	s := SubSource{Name: "src3", URL: srv.URL, SubcriptionID: sub.ID, Group: "my", Enabled: true, Interval: 600}
	if err := DB.Create(&s).Error; err != nil {
		t.Fatalf("创建源失败: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp := s
			if err := cp.Refresh(false); err != nil {
				t.Logf("并发刷新返回错误（可接受）: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("并发 10 次应只抓取 1 次，实际 %d", got)
	}
	if got := len(nodesOfSource(t, s.ID)); got != 1 {
		t.Errorf("期望 1 个节点（不重复），实际 %d", got)
	}
}

// 全局开关关闭时完全不刷新
func TestRefreshDisabledGlobally(t *testing.T) {
	setupTestDB(t)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	defer srv.Close()

	s := SubSource{Name: "src4", URL: srv.URL, Enabled: true, Interval: 600}
	if err := DB.Create(&s).Error; err != nil {
		t.Fatalf("创建源失败: %v", err)
	}
	SubSourceEnabled = false
	defer func() { SubSourceEnabled = true }()
	if err := s.Refresh(true); err != nil {
		t.Fatalf("开关关闭时不应报错: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("开关关闭时不应抓取，实际 %d", got)
	}
}

// 接管：源里出现「库里已有的同链接手工节点」时，接管它而不是新建重复节点
func TestSyncNodesAdoptExistingManualNode(t *testing.T) {
	setupTestDB(t)
	sub := newTestSub(t, "t-sub-adopt")

	// 手工节点，链接与源里的一致（现实中很常见：先手加，后建源）
	manualLink := "vless://u1@a.com:443?type=tcp&security=tls&sni=a.com#node-1"
	manual := Node{Name: "node-1", Link: manualLink}
	if err := DB.Create(&manual).Error; err != nil {
		t.Fatalf("创建手工节点失败: %v", err)
	}
	if err := DB.Model(&sub).Association("Nodes").Append(&manual); err != nil {
		t.Fatalf("手工节点入订阅失败: %v", err)
	}
	if err := DB.Model(&sub).Update("node_order", "node-1").Error; err != nil {
		t.Fatalf("设置 NodeOrder 失败: %v", err)
	}

	s := SubSource{Name: "src-adopt", URL: "http://example.invalid/x", SubcriptionID: sub.ID, Group: "my", Enabled: true, Interval: 600}
	if err := DB.Create(&s).Error; err != nil {
		t.Fatalf("创建源失败: %v", err)
	}
	if err := s.syncNodes([]string{manualLink}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	// 不应该出现重复节点
	var count int64
	if err := DB.Model(&Node{}).Where("name = ?", "node-1").Count(&count).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Errorf("同链接节点应被接管而不是新建，实际有 %d 个同名节点", count)
	}
	var got Node
	if err := DB.Where("name = ?", "node-1").First(&got).Error; err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	if got.SourceID != s.ID {
		t.Errorf("接管后 SourceID 应为 %d，实际 %d", s.ID, got.SourceID)
	}
	if got.ID != manual.ID {
		t.Errorf("接管应该是原地更新（id 不变），原 id=%d 现 id=%d", manual.ID, got.ID)
	}
}

// LinkKey：改名不算换节点
func TestLinkKey(t *testing.T) {
	if got := LinkKey("vless://u@a.com:443?type=tcp#名字"); got != "vless://u@a.com:443?type=tcp" {
		t.Errorf("LinkKey 未去掉 fragment: %s", got)
	}
	if got := LinkKey("  vless://u@a.com:443?type=tcp  "); got != "vless://u@a.com:443?type=tcp" {
		t.Errorf("LinkKey 未 trim: %q", got)
	}
	fmt.Sprint() // 保持 fmt 引用（断言信息里可能用到）
}
