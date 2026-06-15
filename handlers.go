package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"github.com/gin-gonic/gin"
	"github.com/shiblon/entroq/service"
)

// GetEntroqHandler handles GET requests to /entroq
func GetEntroqHandler(c *gin.Context) {
	entroq, err := service.GetEntroq(context.Background())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, entroq)
}
