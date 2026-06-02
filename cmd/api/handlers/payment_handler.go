package handlers

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	paymentapi "github.com/monorepo/internal/modules/payment/api"
	"github.com/monorepo/internal/platform/apperror"
)

type PaymentHandler struct {
	svc      paymentapi.Service
	validate *validator.Validate
}

func NewPaymentHandler(svc paymentapi.Service) *PaymentHandler {
	return &PaymentHandler{svc: svc, validate: validator.New()}
}

func (h *PaymentHandler) Register(r fiber.Router) {
	g := r.Group("/payments")
	g.Post("/", h.create)
	g.Get("/:id", h.getByID)
}

func (h *PaymentHandler) create(c *fiber.Ctx) error {
	var req createPaymentRequest
	if err := c.BodyParser(&req); err != nil {
		return apperror.Invalid("invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return apperror.Invalid(err.Error())
	}
	from, _ := uuid.Parse(req.FromAccountID)
	to, _ := uuid.Parse(req.ToAccountID)

	p, err := h.svc.CreatePayment(c.UserContext(), paymentapi.CreatePaymentInput{
		FromAccountID: from,
		ToAccountID:   to,
		Amount:        req.Amount,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(paymentToResponse(p))
}

func (h *PaymentHandler) getByID(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperror.Invalid("invalid payment id")
	}
	p, err := h.svc.GetByID(c.UserContext(), id)
	if err != nil {
		return err
	}
	return c.JSON(paymentToResponse(p))
}

type createPaymentRequest struct {
	FromAccountID string `json:"from_account_id" validate:"required,uuid"`
	ToAccountID   string `json:"to_account_id" validate:"required,uuid"`
	Amount        int64  `json:"amount" validate:"required,gt=0"`
}

type paymentResponse struct {
	ID            string `json:"id"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
}

func paymentToResponse(p paymentapi.Payment) paymentResponse {
	return paymentResponse{
		ID:            p.ID.String(),
		FromAccountID: p.FromAccountID.String(),
		ToAccountID:   p.ToAccountID.String(),
		Amount:        p.Amount,
		Currency:      p.Currency,
		Status:        p.Status,
	}
}
