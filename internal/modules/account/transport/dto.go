package transport

import "github.com/monorepo/internal/modules/account/domain"

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

func toResponse(a *domain.Account) accountResponse {
	return accountResponse{
		ID:       a.ID.String(),
		OwnerID:  a.OwnerID.String(),
		Currency: a.Currency,
		Balance:  a.Balance,
	}
}
