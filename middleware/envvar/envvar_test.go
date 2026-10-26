package envvar

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

func Test_EnvVarStructWithExportVars(t *testing.T) {
	t.Setenv("testKey", "testEnvValue")
	t.Setenv("anotherEnvKey", "anotherEnvVal")
	vars := newEnvVar(Config{
		ExportVars: map[string]string{"testKey": "", "testDefaultKey": "testDefaultVal"},
	})

	require.Equal(t, "testEnvValue", vars.Vars["testKey"])
	require.Equal(t, "testDefaultVal", vars.Vars["testDefaultKey"])
	require.Empty(t, vars.Vars["anotherEnvKey"])
}

func Test_EnvVarHandler(t *testing.T) {
	t.Setenv("testKey", "testVal")

	expectedEnvVarResponse, err := json.Marshal(
		struct {
			Vars map[string]string `json:"vars"`
		}{
			Vars: map[string]string{"testKey": "testVal"},
		},
	)
	require.NoError(t, err)

	app := fiber.New()
	app.Use("/envvars", New(Config{
		ExportVars: map[string]string{"testKey": ""},
	}))

	req, err := http.NewRequestWithContext(context.Background(), fiber.MethodGet, "http://localhost/envvars", http.NoBody)
	require.NoError(t, err)
	resp := apptest.Do(t, app, req)

	respBody := apptest.BodyBytes(t, resp)

	require.Equal(t, expectedEnvVarResponse, respBody)
}

func Test_EnvVarHandlerNotMatched(t *testing.T) {
	app := fiber.New()
	app.Use("/envvars", New(Config{
		ExportVars: map[string]string{"testKey": ""},
	}))

	app.Get("/another-path", func(ctx fiber.Ctx) error {
		require.NoError(t, ctx.SendString("OK"))
		return nil
	})

	req, err := http.NewRequestWithContext(context.Background(), fiber.MethodGet, "http://localhost/another-path", http.NoBody)
	require.NoError(t, err)
	resp := apptest.Do(t, app, req)

	respBody := apptest.BodyBytes(t, resp)

	require.Equal(t, []byte("OK"), respBody)
}

func Test_EnvVarHandlerDefaultConfig(t *testing.T) {
	t.Setenv("testEnvKey", "testEnvVal")

	app := fiber.New()
	app.Use("/envvars", New())

	req, err := http.NewRequestWithContext(context.Background(), fiber.MethodGet, "http://localhost/envvars", http.NoBody)
	require.NoError(t, err)
	resp := apptest.Do(t, app, req)

	respBody := apptest.BodyBytes(t, resp)

	var envVars EnvVar
	require.NoError(t, json.Unmarshal(respBody, &envVars))
	_, exists := envVars.Vars["testEnvKey"]
	require.False(t, exists)
}

func Test_EnvVarHandlerMethod(t *testing.T) {
	app := fiber.New()
	app.Use("/envvars", New())

	req, err := http.NewRequestWithContext(context.Background(), fiber.MethodPost, "http://localhost/envvars", http.NoBody)
	require.NoError(t, err)
	resp := apptest.Expect(t, app, req, fiber.StatusMethodNotAllowed)
	require.Equal(t, hAllow, resp.Header.Get(fiber.HeaderAllow))
}

func Test_EnvVarHandlerHead(t *testing.T) {
	app := fiber.New()
	app.Use("/envvars", New())

	resp := apptest.Expect(t, app, apptest.Req(fiber.MethodHead, "http://localhost/envvars"), fiber.StatusOK)

	body := apptest.Body(t, resp)
	require.Empty(t, body)
}

func Test_EnvVarHandlerSpecialValue(t *testing.T) {
	testEnvKey := "testEnvKey"
	fakeBase64 := "testBase64:TQ=="
	t.Setenv(testEnvKey, fakeBase64)

	app := fiber.New()
	app.Use("/envvars/export", New(Config{ExportVars: map[string]string{testEnvKey: ""}}))
	app.Use("/envvars", New())

	req, err := http.NewRequestWithContext(context.Background(), fiber.MethodGet, "http://localhost/envvars", http.NoBody)
	require.NoError(t, err)
	resp := apptest.Do(t, app, req)

	respBody := apptest.BodyBytes(t, resp)

	var envVars EnvVar
	require.NoError(t, json.Unmarshal(respBody, &envVars))
	_, exists := envVars.Vars[testEnvKey]
	require.False(t, exists)

	req, err = http.NewRequestWithContext(context.Background(), fiber.MethodGet, "http://localhost/envvars/export", http.NoBody)
	require.NoError(t, err)
	resp = apptest.Do(t, app, req)

	respBody = apptest.BodyBytes(t, resp)

	var envVarsExport EnvVar
	require.NoError(t, json.Unmarshal(respBody, &envVarsExport))
	val := envVarsExport.Vars[testEnvKey]
	require.Equal(t, fakeBase64, val)
}
