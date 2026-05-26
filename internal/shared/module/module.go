// Package module defines the contract every business module implements so the
// composition root can treat modules uniformly.
package module

import (
	"io/fs"

	"github.com/gofiber/fiber/v2"
)

// Module is a self-contained vertical slice of the monolith.
type Module interface {
	// Name is the module identifier and the name of its dedicated DB schema.
	Name() string
	// RegisterRoutes attaches the module's HTTP routes to the shared router.
	RegisterRoutes(r fiber.Router)
	// Migrations returns the embedded SQL migrations for the module's schema.
	Migrations() fs.FS
}
