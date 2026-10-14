package routers

import (
	"sublink/api"

	"github.com/gin-gonic/gin"
)

// 订阅源相关路由（需要登录态，走全局 AuthorToken 中间件）
func Sources(r *gin.Engine) {
	SourceGroup := r.Group("/api/v1/source")
	{
		SourceGroup.GET("/get", api.SourceGet)
		SourceGroup.POST("/add", api.SourceAdd)
		SourceGroup.POST("/update", api.SourceUpdate)
		SourceGroup.DELETE("/delete", api.SourceDel)
		SourceGroup.POST("/refresh", api.SourceRefresh)
	}
}
