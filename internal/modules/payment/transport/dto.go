package transport

import "github.com/monorepo/internal/modules/payment/domain"

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

func toResponse(p *domain.Payment) paymentResponse {
	return paymentResponse{
		ID:            p.ID.String(),
		FromAccountID: p.FromAccountID.String(),
		ToAccountID:   p.ToAccountID.String(),
		Amount:        p.Amount,
		Currency:      p.Currency,
		Status:        string(p.Status),
	}
}
