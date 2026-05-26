// Package transport exposes the account module over HTTP. Handlers stay thin:
// they parse and validate input, call the service through its interface, and
// map the result back. The Fiber framework never leaks into the service layer.
package transport

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/monorepo/internal/modules/account/service"
	"github.com/monorepo/internal/shared/apperror"
)

type Handler struct {
	svc      service.Service
	validate *validator.Validate
}

func NewHandler(svc service.Service) *Handler {
	return &Handler{svc: svc, validate: validator.New()}
}

func (h *Handler) Register(r fiber.Router) {
	g := r.Group("/accounts")
	g.Post("/", h.create)
	g.Get("/:id", h.getByID)
}

func (h *Handler) create(c *fiber.Ctx) error {
	var req createAccountRequest
	if err := c.BodyParser(&req); err != nil {
		return apperror.Invalid("invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return apperror.Invalid(err.Error())
	}
	ownerID, _ := uuid.Parse(req.OwnerID) // already validated as a UUID above

	acc, err := h.svc.CreateAccount(c.UserContext(), ownerID, req.Currency)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(toResponse(acc))
}

func (h *Handler) getByID(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperror.Invalid("invalid account id")
	}
	acc, err := h.svc.GetAccount(c.UserContext(), id)
	if err != nil {
		return err
	}
	return c.JSON(toResponse(acc))
}
