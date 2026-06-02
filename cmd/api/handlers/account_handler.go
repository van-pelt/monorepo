// Package handlers exposes the application's HTTP endpoints. Each handler
// depends only on a module's public api.Service — never on the module's
// internals. Swapping the in-process api for a gRPC client at the composition
// root requires no change here.
package handlers

import (
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	accountapi "github.com/monorepo/internal/modules/account/api"
	"github.com/monorepo/internal/platform/apperror"
)

type AccountHandler struct {
	svc      accountapi.Service
	validate *validator.Validate
}

func NewAccountHandler(svc accountapi.Service) *AccountHandler {
	return &AccountHandler{svc: svc, validate: validator.New()}
}

func (h *AccountHandler) Register(r fiber.Router) {
	g := r.Group("/accounts")
	g.Post("/", h.create)
	g.Get("/:id", h.getByID)
}

func (h *AccountHandler) create(c *fiber.Ctx) error {
	var req createAccountRequest
	if err := c.BodyParser(&req); err != nil {
		return apperror.Invalid("invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return apperror.Invalid(err.Error())
	}
	ownerID, _ := uuid.Parse(req.OwnerID) // already validated as a UUID above

	acc, err := h.svc.CreateAccount(c.UserContext(), accountapi.CreateAccountInput{
		OwnerID:  ownerID,
		Currency: req.Currency,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(accountToResponse(acc))
}

func (h *AccountHandler) getByID(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperror.Invalid("invalid account id")
	}
	acc, err := h.svc.GetByID(c.UserContext(), id)
	if err != nil {
		return err
	}
	return c.JSON(accountToResponse(acc))
}

type createAccountRequest struct {
	OwnerID  string `json:"owner_id" validate:"required,uuid"`
	Currency string `json:"currency" validate:"required,len=3"`
}

type accountResponse struct {
	ID       string `json:"id"`
	OwnerID  string `json:"owner_id"`
	Currency string `json:"currency"`
	Balance  int64  `json:"balance"`
}

func accountToResponse(a accountapi.Account) accountResponse {
	return accountResponse{
		ID:       a.ID.String(),
		OwnerID:  a.OwnerID.String(),
		Currency: a.Currency,
		Balance:  a.Balance,
	}
}
