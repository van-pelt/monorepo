// Package transport exposes the payment module over HTTP.
package transport

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/monorepo/internal/modules/payment/service"
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
	g := r.Group("/payments")
	g.Post("/", h.create)
	g.Get("/:id", h.getByID)
}

func (h *Handler) create(c *fiber.Ctx) error {
	var req createPaymentRequest
	if err := c.BodyParser(&req); err != nil {
		return apperror.Invalid("invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return apperror.Invalid(err.Error())
	}
	from, _ := uuid.Parse(req.FromAccountID) // validated as UUID above
	to, _ := uuid.Parse(req.ToAccountID)

	p, err := h.svc.CreatePayment(c.UserContext(), service.CreatePaymentInput{
		FromAccountID: from,
		ToAccountID:   to,
		Amount:        req.Amount,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(toResponse(p))
}

func (h *Handler) getByID(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperror.Invalid("invalid payment id")
	}
	p, err := h.svc.GetPayment(c.UserContext(), id)
	if err != nil {
		return err
	}
	return c.JSON(toResponse(p))
}
