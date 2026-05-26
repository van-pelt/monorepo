package httpserver

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/shared/apperror"
)

type errorResponse struct {
	Error string `json:"error"`
}

// errorHandler is the single place that translates errors into HTTP responses.
// Business code returns apperror.Error values; it never references HTTP codes.
func errorHandler(log zerolog.Logger) fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		status := fiber.StatusInternalServerError
		msg := "internal server error"

		var appErr *apperror.Error
		var fiberErr *fiber.Error
		switch {
		case errors.As(err, &appErr):
			switch appErr.Kind {
			case apperror.KindNotFound:
				status, msg = fiber.StatusNotFound, appErr.Msg
			case apperror.KindInvalid:
				status, msg = fiber.StatusBadRequest, appErr.Msg
			case apperror.KindConflict:
				status, msg = fiber.StatusConflict, appErr.Msg
			default:
				status = fiber.StatusInternalServerError
			}
		case errors.As(err, &fiberErr):
			status, msg = fiberErr.Code, fiberErr.Message
		}

		if status >= fiber.StatusInternalServerError {
			log.Error().Err(err).Str("path", c.Path()).Msg("request failed")
		}
		return c.Status(status).JSON(errorResponse{Error: msg})
	}
}
