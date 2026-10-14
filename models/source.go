package models

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"sublink/node"

	"gorm.io/gorm"
)

// SubSource 订阅源：把上游订阅地址（x-ui 的 /jjsson|/ssuubb/<subId> 等）
// 登记成一个源，绑定到一个订阅；订阅被请求时按间隔惰性刷新。
type SubSource struct {
	gorm.Model
	ID            int
	Name          string    // 显示名
	URL           string    // 源地址
	UA            string    // 抓取用 UA（默认 mihomo/1.19.31，避免拿到 HTML 订阅页）
	SubcriptionID int       // 绑定订阅（一对一）
	Group         string    // 导入节点归属分组，默认 my
	Interval      int       // 刷新间隔（秒），默认 600
	Prune         bool      // 源里消失的节点是否从订阅移除（默认 false）
	Enabled       bool
	LastAt        time.Time // 上次「尝试」刷新时间（失败也推进，用于退避）
	LastOK        bool
	LastMsg       string
}

const (
	// DefaultSourceUA 抓取上游时使用的 UA。
	// 上游（x-ui）会按 UA / Accept 返回不同内容：浏览器 Accept: text/html 会拿到 HTML 订阅页。
	DefaultSourceUA = "mihomo/1.19.31"
	// DefaultSourceInterval 默认刷新间隔（秒）
	DefaultSourceInterval = 600
	// DefaultSourceGroup 导入节点默认分组
	DefaultSourceGroup = "my"
	// sourceTimeout 抓取超时
	sourceTimeout = 5 * time.Second
	// sourceMaxBody 响应体上限（防跑偏）
	sourceMaxBody = 2 << 20
)

// SubSourceEnabled 全局开关，出问题可一键关掉刷新
var SubSourceEnabled = true

// sourceLocks 保证同一个源并发请求时只真的抓取一次
var sourceLocks sync.Map // map[int]*sync.Mutex

func sourceLock(id int) *sync.Mutex {
	v, _ := sourceLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ---------- CRUD ----------

func (s *SubSource) List() ([]SubSource, error) {
	var list []SubSource
	err := DB.Find(&list).Error
	return list, err
}

func (s *SubSource) Find() error {
	if s.ID == 0 && s.Name == "" {
		return errors.New("id 和 name 不能同时为空")
	}
	return DB.Where("id = ? or name = ?", s.ID, s.Name).First(s).Error
}

func (s *SubSource) Add() error {
	if strings.TrimSpace(s.URL) == "" {
		return errors.New("源地址不能为空")
	}
	if s.UA == "" {
		s.UA = DefaultSourceUA
	}
	if s.Interval <= 0 {
		s.Interval = DefaultSourceInterval
	}
	if strings.TrimSpace(s.Group) == "" {
		s.Group = DefaultSourceGroup
	}
	return DB.Create(s).Error
}

func (s *SubSource) Update(new *SubSource) error {
	var old SubSource
	if err := DB.Where("id = ?", s.ID).First(&old).Error; err != nil {
		return err
	}
	if new.Name != "" {
		old.Name = new.Name
	}
	if new.URL != "" {
		old.URL = new.URL
	}
	if new.UA != "" {
		old.UA = new.UA
	}
	if new.Group != "" {
		old.Group = new.Group
	}
	if new.Interval > 0 {
		old.Interval = new.Interval
	}
	old.SubcriptionID = new.SubcriptionID
	old.Prune = new.Prune
	old.Enabled = new.Enabled
	return DB.Save(&old).Error
}

// Del 删除源；delNodes=true 时同时删除该源导入的节点
func (s *SubSource) Del(delNodes bool) error {
	if delNodes {
		var nodes []Node
		if err := DB.Where("source_id = ?", s.ID).Find(&nodes).Error; err == nil {
			for i := range nodes {
				if err := nodes[i].Del(); err != nil {
					log.Println("[订阅源] 删除节点失败:", nodes[i].Name, err)
				}
			}
			if s.SubcriptionID > 0 {
				if err := removeNodesFromSubcription(s.SubcriptionID, nodes); err != nil {
					log.Println("[订阅源] 从订阅移除节点失败:", err)
				}
			}
		}
	}
	return DB.Delete(s).Error
}

// ---------- 刷新（请求驱动） ----------

// Refresh 按间隔惰性刷新；force=true 忽略间隔。
// 失败时只记录状态并沿用库里旧节点（fail-open），绝不返回网络错误给调用方。
func (s *SubSource) Refresh(force bool) error {
	if !SubSourceEnabled || !s.Enabled || strings.TrimSpace(s.URL) == "" {
		return nil
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSourceInterval
	}
	// 快路径：未到间隔（零网络请求）
	if !force && !s.LastAt.IsZero() && time.Since(s.LastAt) < time.Duration(interval)*time.Second {
		return nil
	}

	lock := sourceLock(s.ID)
	lock.Lock()
	defer lock.Unlock()
	// 等锁期间可能已经被别的请求刷过：以库里的 LastAt 为准再判一次
	// （并发请求各自持有的是自己的结构体副本，只判内存会重复抓取）
	if !force {
		var fresh SubSource
		if err := DB.Select("last_at").Where("id = ?", s.ID).First(&fresh).Error; err == nil && !fresh.LastAt.IsZero() {
			s.LastAt = fresh.LastAt
			if time.Since(s.LastAt) < time.Duration(interval)*time.Second {
				return nil
			}
		}
	}

	links, err := FetchSourceLinks(s.URL, s.UA)
	s.LastAt = time.Now()
	if err != nil {
		s.LastOK, s.LastMsg = false, truncateMsg(err.Error())
		s.saveState()
		log.Println("[订阅源] 刷新失败，沿用库里旧节点:", s.Name, err)
		return err
	}
	if err := s.syncNodes(links); err != nil {
		s.LastOK, s.LastMsg = false, truncateMsg(err.Error())
		s.saveState()
		log.Println("[订阅源] 同步节点失败，沿用库里旧节点:", s.Name, err)
		return err
	}
	s.LastOK = true
	s.LastMsg = fmt.Sprintf("成功，共 %d 个节点", len(links))
	s.saveState()
	log.Println("[订阅源] 刷新成功:", s.Name, s.LastMsg)
	return nil
}

// RefreshForSubcription 订阅被请求时调用：刷新绑定到它的所有源
func RefreshForSubcription(sub *Subcription) {
	if !SubSourceEnabled || sub == nil || sub.ID == 0 {
		return
	}
	var list []SubSource
	if err := DB.Where("subcription_id = ? and enabled = ?", sub.ID, true).Find(&list).Error; err != nil {
		log.Println("[订阅源] 查询订阅源失败:", err)
		return
	}
	for i := range list {
		if err := list[i].Refresh(false); err != nil {
			// 已经记过日志了，这里只是保证不影响订阅响应
			continue
		}
	}
}

func (s *SubSource) saveState() {
	err := DB.Model(s).Updates(map[string]interface{}{
		"last_at":  s.LastAt,
		"last_ok":  s.LastOK,
		"last_msg": truncateMsg(s.LastMsg),
	}).Error
	if err != nil {
		log.Println("[订阅源] 写入刷新状态失败:", err)
	}
}

// FetchSourceLinks 抓取上游订阅并转换成分享链接
func FetchSourceLinks(rawurl, ua string) ([]string, error) {
	if ua == "" {
		ua = DefaultSourceUA
	}
	client := &http.Client{Timeout: sourceTimeout}
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	// 关键：固定客户端 UA、不要 Accept: text/html，否则上游会返回 HTML 订阅页
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, sourceMaxBody))
	if err != nil {
		return nil, err
	}
	return node.DecodeSubPayload(body)
}

// syncNodes 把源里的链接对账进库，并同步到绑定的订阅
func (s *SubSource) syncNodes(links []string) error {
	group := s.Group
	if strings.TrimSpace(group) == "" {
		group = DefaultSourceGroup
	}

	var existing []Node
	if err := DB.Where("source_id = ?", s.ID).Find(&existing).Error; err != nil {
		return err
	}
	byKey := map[string]*Node{}
	for i := range existing {
		byKey[LinkKey(existing[i].Link)] = &existing[i]
	}
	// 全库按链接索引：用于「已有同链接节点」的接管判断
	var allNodes []Node
	if err := DB.Find(&allNodes).Error; err != nil {
		return err
	}
	allByKey := map[string]*Node{}
	for i := range allNodes {
		allByKey[LinkKey(allNodes[i].Link)] = &allNodes[i]
	}

	var added, updated, adopted []Node
	seen := map[string]bool{}
	for _, link := range links {
		key := LinkKey(link)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		name := nodeNameFromLink(link)
		if cur, ok := byKey[key]; ok {
			if cur.Link != link || cur.Name != name {
				cur.Link, cur.Name = link, name
				if err := DB.Model(cur).Updates(map[string]interface{}{"link": link, "name": name}).Error; err != nil {
					log.Println("[订阅源] 更新节点失败:", err)
					continue
				}
				updated = append(updated, *cur)
			}
			continue
		}

		// 库里已经有同链接的节点（手工加的、或另一个源带来的）
		if other, ok := allByKey[key]; ok {
			switch other.SourceID {
			case 0, s.ID:
				// 手工节点 → 接管；本源节点 → 更新（理论上不会走到这里）
				if err := DB.Model(other).Updates(map[string]interface{}{
					"link": link, "name": name, "source_id": s.ID,
				}).Error; err != nil {
					log.Println("[订阅源] 接管已有节点失败:", err)
					continue
				}
				other.Link, other.Name, other.SourceID = link, name, s.ID
				adopted = append(adopted, *other)
				if err := bindNodeToGroup(other, group); err != nil {
					log.Println("[订阅源] 分组关联失败:", err)
				}
			default:
				// 同链接已被别的源管理，避免互相覆盖
				log.Println("[订阅源] 同链接节点属于其它源，跳过:", other.Name)
			}
			continue
		}

		n := Node{Name: name, Link: link, SourceID: s.ID}
		if err := DB.Create(&n).Error; err != nil {
			log.Println("[订阅源] 新增节点失败:", err)
			continue
		}
		if err := bindNodeToGroup(&n, group); err != nil {
			log.Println("[订阅源] 分组关联失败:", err)
		}
		added = append(added, n)
	}

	// Prune：源里已经消失的节点（默认关闭）
	if s.Prune {
		var gone []Node
		for i := range existing {
			if !seen[LinkKey(existing[i].Link)] {
				gone = append(gone, existing[i])
			}
		}
		for i := range gone {
			if err := gone[i].Del(); err != nil {
				log.Println("[订阅源] 清理节点失败:", gone[i].Name, err)
			}
		}
		if len(gone) > 0 && s.SubcriptionID > 0 {
			if err := removeNodesFromSubcription(s.SubcriptionID, gone); err != nil {
				log.Println("[订阅源] 从订阅移除已消失节点失败:", err)
			}
		}
	}

	// 新节点（含接管的手工节点）并入绑定的订阅
	candidates := append(added, adopted...)
	if len(candidates) > 0 && s.SubcriptionID > 0 {
		if err := appendNodesToSubcription(s.SubcriptionID, candidates); err != nil {
			return err
		}
	}
	if len(added) > 0 || len(updated) > 0 || len(adopted) > 0 {
		log.Printf("[订阅源] %s：新增 %d 个，更新 %d 个，接管 %d 个", s.Name, len(added), len(updated), len(adopted))
	}
	return nil
}

// bindNodeToGroup 把节点关联到分组（分组不存在则创建）
func bindNodeToGroup(n *Node, group string) error {
	if strings.TrimSpace(group) == "" {
		return nil
	}
	gn := &GroupNode{Name: group}
	if err := gn.Add(); err != nil {
		return err
	}
	return gn.Ass(n)
}

// ---------- 订阅侧同步 ----------
//
// 注意：Subcription.Find() 会按 NodeOrder 重排并「丢掉」不在 NodeOrder 里的节点，
// 所以往订阅加节点时必须同步维护 NodeOrder。

func appendNodesToSubcription(subID int, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	var sub Subcription
	if err := DB.Preload("Nodes").Where("id = ?", subID).First(&sub).Error; err != nil {
		return err
	}
	if err := DB.Model(&sub).Association("Nodes").Append(nodes); err != nil {
		return err
	}
	order := splitOrder(sub.NodeOrder)
	have := map[string]bool{}
	for _, name := range order {
		have[name] = true
	}
	for _, n := range nodes {
		if !have[n.Name] {
			order = append(order, n.Name)
			have[n.Name] = true
		}
	}
	return DB.Model(&sub).Update("node_order", strings.Join(order, ",")).Error
}

func removeNodesFromSubcription(subID int, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	var sub Subcription
	if err := DB.Preload("Nodes").Where("id = ?", subID).First(&sub).Error; err != nil {
		return err
	}
	var kept []Node
	drop := map[string]bool{}
	for _, n := range nodes {
		drop[n.Name] = true
	}
	for _, n := range sub.Nodes {
		if !drop[n.Name] {
			kept = append(kept, n)
		}
	}
	if err := DB.Model(&sub).Association("Nodes").Replace(kept); err != nil {
		return err
	}
	order := splitOrder(sub.NodeOrder)
	var newOrder []string
	for _, name := range order {
		if !drop[name] {
			newOrder = append(newOrder, name)
		}
	}
	return DB.Model(&sub).Update("node_order", strings.Join(newOrder, ",")).Error
}

func splitOrder(order string) []string {
	var out []string
	for _, v := range strings.Split(order, ",") {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------- 工具 ----------

// LinkKey 节点的去重键：去掉 #fragment 后的链接（改名不算换节点）
func LinkKey(link string) string {
	if i := strings.Index(link, "#"); i >= 0 {
		return strings.TrimSpace(link[:i])
	}
	return strings.TrimSpace(link)
}

// nodeNameFromLink 取链接的 #fragment 当作节点名
func nodeNameFromLink(link string) string {
	if i := strings.Index(link, "#"); i >= 0 {
		raw := link[i+1:]
		if name, err := url.QueryUnescape(raw); err == nil && strings.TrimSpace(name) != "" {
			return name
		}
		return raw
	}
	return ""
}

func truncateMsg(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200])
	}
	return s
}
