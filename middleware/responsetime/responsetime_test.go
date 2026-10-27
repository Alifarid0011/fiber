package responsetime

import (
	"errors"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

func TestResponseTimeMiddleware(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	tests := []struct {
		name                  string
		expectedStatus        int
		useCustomErrorHandler bool
		returnError           bool
		expectHeader          bool
		skipWithNext          bool
	}{
		{
			name:           "sets duration header",
			expectedStatus: fiber.StatusOK,
			expectHeader:   true,
		},
		{
			name:           "skips when Next returns true",
			expectedStatus: fiber.StatusOK,
			expectHeader:   false,
			skipWithNext:   true,
		},
		{
			name:                  "propagates errors",
			expectedStatus:        fiber.StatusTeapot,
			useCustomErrorHandler: true,
			returnError:           true,
			expectHeader:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configs := []Config(nil)
			if tt.skipWithNext {
				configs = []Config{{
					Next: func(fiber.Ctx) bool { return true },
				}}
			}

			appConfig := fiber.Config{}
			if tt.useCustomErrorHandler {
				appConfig.ErrorHandler = func(c fiber.Ctx, err error) error {
					t.Helper()
					require.ErrorIs(t, err, boom)

					return c.Status(fiber.StatusTeapot).SendString(err.Error())
				}
			}

			app := fiber.New(appConfig)
			app.Use(New(configs...))

			app.Get("/", func(c fiber.Ctx) error {
				if tt.returnError {
					return boom
				}

				return c.SendStatus(fiber.StatusOK)
			})

			resp := apptest.Expect(t, app, "/", tt.expectedStatus)

			header := resp.Header.Get(fiber.HeaderXResponseTime)
			if tt.expectHeader {
				require.NotEmpty(t, header)

				_, parseErr := time.ParseDuration(header)
				require.NoError(t, parseErr)

				return
			}

			require.Empty(t, header)
		})
	}
}
