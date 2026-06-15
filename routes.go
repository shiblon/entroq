package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/shiblon/entroq/handlers"
)

// DefineRoutes defines the REST API endpoints
func DefineRoutes(r *gin.RouterGroup) {
	r.GET("/entroq", handlers.GetEntroqHandler)
}
