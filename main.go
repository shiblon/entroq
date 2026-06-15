package main

import (
	"encoding/json"
	"log"
	"net/http"
	"github.com/gin-gonic/gin"
	"github.com/shiblon/entroq/service"
)

func main() {
	// Create a new Gin router
	r := gin.Default()

	// Initialize the entroq service
	service.Init()

	// Define REST API endpoints
	routes.DefineRoutes(r)

	// Start the server
	r.Run(":8080")
}
