package api

import (
	"log"
	"strconv"
	"strings"

	"sublink/models"

	"github.com/gin-gonic/gin"
)

// 订阅源管理接口（/api/v1/source/*）
//
// 用法示例：
//
//	POST /api/v1/source/add
//	  name=x-ui 主订阅
//	  url=https://panel.example.com/jjsson/<subId>
//	  subcription=sublink-x        （订阅名或 id，一对一绑定）
//	  group=my                     （导入节点归属分组，默认 my）
//	  interval=600                 （刷新间隔秒，默认 600）
//	  prune=false                  （源里消失的节点是否移除，默认 false）
//	  enabled=true
//	  ua=mihomo/1.19.31            （可选，抓取用 UA）

// SourceGet 列出订阅源（可选按 id / name 查单个）
func SourceGet(c *gin.Context) {
	id := c.Query("id")
	name := c.Query("name")
	if id != "" || name != "" {
		var s models.SubSource
		if id != "" {
			s.ID, _ = strconv.Atoi(id)
		}
		s.Name = name
		if err := s.Find(); err != nil {
			c.JSON(400, gin.H{"msg": "找不到这个订阅源"})
			return
		}
		c.JSON(200, gin.H{"code": "00000", "data": s, "msg": "ok"})
		return
	}
	var s models.SubSource
	list, err := s.List()
	if err != nil {
		c.JSON(500, gin.H{"msg": err.Error()})
		return
	}
	c.JSON(200, gin.H{"code": "00000", "data": list, "msg": "ok"})
}

// SourceAdd 新增订阅源
func SourceAdd(c *gin.Context) {
	s, err := sourceFromForm(c)
	if err != nil {
		c.JSON(400, gin.H{"msg": err.Error()})
		return
	}
	if err := s.Add(); err != nil {
		log.Println("添加订阅源失败:", err)
		c.JSON(400, gin.H{"msg": err.Error()})
		return
	}
	c.JSON(200, gin.H{"code": "00000", "msg": "添加成功", "data": s})
}

// SourceUpdate 更新订阅源
func SourceUpdate(c *gin.Context) {
	id, _ := strconv.Atoi(c.PostForm("id"))
	if id == 0 {
		c.JSON(400, gin.H{"msg": "id 不能为空"})
		return
	}
	s := models.SubSource{ID: id}
	if err := s.Find(); err != nil {
		c.JSON(400, gin.H{"msg": "找不到这个订阅源"})
		return
	}
	newSource, err := sourceFromForm(c)
	if err != nil {
		c.JSON(400, gin.H{"msg": err.Error()})
		return
	}
	if err := s.Update(&newSource); err != nil {
		log.Println("更新订阅源失败:", err)
		c.JSON(400, gin.H{"msg": err.Error()})
		return
	}
	c.JSON(200, gin.H{"code": "00000", "msg": "更新成功"})
}

// SourceDel 删除订阅源（delNodes=true 时连带删除它导入的节点）
func SourceDel(c *gin.Context) {
	id, _ := strconv.Atoi(c.Query("id"))
	if id == 0 {
		c.JSON(400, gin.H{"msg": "id 不能为空"})
		return
	}
	s := models.SubSource{ID: id}
	if err := s.Find(); err != nil {
		c.JSON(400, gin.H{"msg": "找不到这个订阅源"})
		return
	}
	delNodes := c.Query("delNodes") == "true" || c.Query("delNodes") == "1"
	if err := s.Del(delNodes); err != nil {
		log.Println("删除订阅源失败:", err)
		c.JSON(400, gin.H{"msg": err.Error()})
		return
	}
	c.JSON(200, gin.H{"code": "00000", "msg": "删除成功"})
}

// SourceRefresh 立即刷新（id 为空则刷新全部）
func SourceRefresh(c *gin.Context) {
	id := c.PostForm("id")
	if id == "" {
		id = c.Query("id")
	}
	if id != "" {
		x, _ := strconv.Atoi(id)
		s := models.SubSource{ID: x}
		if err := s.Find(); err != nil {
			c.JSON(400, gin.H{"msg": "找不到这个订阅源"})
			return
		}
		if err := s.Refresh(true); err != nil {
			c.JSON(200, gin.H{"code": "00000", "msg": "刷新失败（已保留旧节点）: " + err.Error(), "lastOK": false})
			return
		}
		c.JSON(200, gin.H{"code": "00000", "msg": "刷新成功: " + s.LastMsg, "lastOK": true})
		return
	}

	var s models.SubSource
	list, err := s.List()
	if err != nil {
		c.JSON(500, gin.H{"msg": err.Error()})
		return
	}
	ok, fail := 0, 0
	var msgs []string
	for i := range list {
		if err := list[i].Refresh(true); err != nil {
			fail++
			msgs = append(msgs, list[i].Name+": "+err.Error())
			continue
		}
		ok++
	}
	c.JSON(200, gin.H{
		"code":   "00000",
		"msg":    "刷新完成",
		"ok":     ok,
		"failed": fail,
		"detail": strings.Join(msgs, " | "),
	})
}

// sourceFromForm 从表单构造订阅源
func sourceFromForm(c *gin.Context) (models.SubSource, error) {
	var s models.SubSource
	s.Name = strings.TrimSpace(c.PostForm("name"))
	s.URL = strings.TrimSpace(c.PostForm("url"))
	s.UA = strings.TrimSpace(c.PostForm("ua"))
	s.Group = strings.TrimSpace(c.PostForm("group"))
	s.Interval, _ = strconv.Atoi(c.PostForm("interval"))
	s.Prune = c.PostForm("prune") == "true" || c.PostForm("prune") == "1"
	// 未显式传 enabled 时默认启用
	if v := c.PostForm("enabled"); v == "" {
		s.Enabled = true
	} else {
		s.Enabled = v == "true" || v == "1"
	}

	// 订阅：支持传 id 或名字
	sub := strings.TrimSpace(c.PostForm("subcription"))
	if sub == "" {
		sub = strings.TrimSpace(c.PostForm("sub"))
	}
	if sub != "" {
		var target models.Subcription
		if n, err := strconv.Atoi(sub); err == nil && n > 0 {
			target.ID = n
		} else {
			target.Name = sub
		}
		if err := target.Find(); err != nil {
			return s, err
		}
		s.SubcriptionID = target.ID
	}
	if s.URL == "" {
		return s, errURLRequired
	}
	return s, nil
}

var errURLRequired = &urlRequiredError{}

type urlRequiredError struct{}

func (e *urlRequiredError) Error() string { return "url 不能为空" }
