package models

import (
	"time"
)

// Entroq represents an entroq object
type Entroq struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}
