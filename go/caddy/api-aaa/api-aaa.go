package apiaaa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/inverse-inc/packetfence/go/api-frontend/aaa"
	"github.com/inverse-inc/packetfence/go/caddy/caddy"
	"github.com/inverse-inc/packetfence/go/caddy/caddy/caddyhttp/httpserver"
	"github.com/inverse-inc/packetfence/go/db"
	"github.com/inverse-inc/packetfence/go/log"
	"github.com/inverse-inc/packetfence/go/panichandler"
	"github.com/inverse-inc/packetfence/go/pfconfigdriver"
	"github.com/inverse-inc/packetfence/go/sharedutils"
	"github.com/inverse-inc/packetfence/go/statsd"
	"github.com/julienschmidt/httprouter"
)

// Register the plugin in caddy
func init() {
	caddy.RegisterPlugin("api-aaa", caddy.Plugin{
		ServerType: "http",
		Action:     setup,
	})
}

type PrettyTokenInfo struct {
	AdminRoles []string `json:"admin_roles"`
	TenantId   int      `json:"tenant_id"`
}

type ApiAAAHandler struct {
	Next           httpserver.Handler
	router         *httprouter.Router
	authentication *aaa.TokenAuthenticationMiddleware
	authorization  *aaa.TokenAuthorizationMiddleware
}

// Setup the api-aaa middleware
// Also loads the pfconfig resources and registers them in the pool
func setup(c *caddy.Controller) error {
	ctx := log.LoggerNewContext(context.Background())

	apiAAA, err := buildApiAAAHandler(ctx)

	if err != nil {
		return err
	}

	httpserver.GetConfig(c).AddMiddleware(func(next httpserver.Handler) httpserver.Handler {
		apiAAA.Next = next
		return apiAAA
	})

	return nil
}

// Build the ApiAAAHandler which will initialize the cache and instantiate the router along with its routes
func buildApiAAAHandler(ctx context.Context) (ApiAAAHandler, error) {

	apiAAA := ApiAAAHandler{}

	pfconfigdriver.PfconfigPool.AddStruct(ctx, &pfconfigdriver.Config.PfConf.Webservices)
	pfconfigdriver.PfconfigPool.AddStruct(ctx, &pfconfigdriver.Config.AdminRoles)

	tokenBackend := aaa.NewMemTokenBackend(1 * time.Hour)
	apiAAA.authentication = aaa.NewTokenAuthenticationMiddleware(tokenBackend)

	// Backend for the pf.conf webservices user
	if pfconfigdriver.Config.PfConf.Webservices.User != "" {
		apiAAA.authentication.AddAuthenticationBackend(aaa.NewMemAuthenticationBackend(
			map[string]string{
				pfconfigdriver.Config.PfConf.Webservices.User: pfconfigdriver.Config.PfConf.Webservices.Pass,
			},
			pfconfigdriver.Config.AdminRoles.Element["ALL"].Actions,
		))
	}

	db, err := db.DbFromConfig(ctx)
	sharedutils.CheckError(err)

	apiAAA.authentication.AddAuthenticationBackend(aaa.NewDbAuthenticationBackend(ctx, db, "api_user"))

	url, err := url.Parse("http://localhost:8080/api/v1/authentication/admin_authentication")
	sharedutils.CheckError(err)
	apiAAA.authentication.AddAuthenticationBackend(aaa.NewPfAuthenticationBackend(ctx, url, false))

	apiAAA.authorization = aaa.NewTokenAuthorizationMiddleware(tokenBackend)

	router := httprouter.New()
	router.POST("/api/v1/login", apiAAA.handleLogin)
	router.GET("/api/v1/token_info", apiAAA.handleTokenInfo)

	apiAAA.router = router

	return apiAAA, nil
}

// Handle an API login
func (h ApiAAAHandler) handleLogin(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	ctx := r.Context()
	defer statsd.NewStatsDTiming(ctx).Send("api-aaa.login")

	var loginParams struct {
		Username string
		Password string
	}

	err := json.NewDecoder(r.Body).Decode(&loginParams)

	if err != nil {
		msg := fmt.Sprintf("Error while decoding payload: %s", err)
		log.LoggerWContext(ctx).Error(msg)
		http.Error(w, fmt.Sprint(err), http.StatusBadRequest)
		return
	}

	auth, token, err := h.authentication.Login(ctx, loginParams.Username, loginParams.Password)

	if auth {
		w.WriteHeader(http.StatusOK)
		res, _ := json.Marshal(map[string]string{
			"token": token,
		})
		fmt.Fprintf(w, string(res))
	} else {
		w.WriteHeader(http.StatusUnauthorized)
		res, _ := json.Marshal(map[string]string{
			"message": err.Error(),
		})
		fmt.Fprintf(w, string(res))
	}
}

// Handle getting the token info
func (h ApiAAAHandler) handleTokenInfo(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	ctx := r.Context()
	defer statsd.NewStatsDTiming(ctx).Send("api-aaa.token_info")

	info := h.authorization.GetTokenInfoFromBearerRequest(ctx, r)

	if info != nil {
		// We'll want to render the roles as an array, not as a map
		prettyInfo := PrettyTokenInfo{
			AdminRoles: make([]string, len(info.AdminRoles)),
			TenantId:   info.TenantId,
		}

		i := 0
		for r, _ := range info.AdminRoles {
			prettyInfo.AdminRoles[i] = r
			i++
		}

		w.WriteHeader(http.StatusOK)
		res, _ := json.Marshal(map[string]interface{}{
			"item": prettyInfo,
		})
		fmt.Fprintf(w, string(res))
	} else {
		w.WriteHeader(http.StatusNotFound)
		res, _ := json.Marshal(map[string]string{
			"message": "Couldn't find any information for the current token. Either it is invalid or it has expired.",
		})
		fmt.Fprintf(w, string(res))
	}
}

func (h ApiAAAHandler) HandleAAA(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	auth, err := h.authentication.BearerRequestIsAuthorized(ctx, r)

	if !auth {
		w.WriteHeader(http.StatusUnauthorized)

		if err == nil {
			err = errors.New("Invalid token. Login again using /api/v1/login")
		}

		res, _ := json.Marshal(map[string]string{
			"message": err.Error(),
		})
		fmt.Fprintf(w, string(res))
		return false
	}

	auth, err = h.authorization.BearerRequestIsAuthorized(ctx, r)

	if auth {
		return true
	} else {
		w.WriteHeader(http.StatusForbidden)
		res, _ := json.Marshal(map[string]string{
			"message": err.Error(),
		})
		fmt.Fprintf(w, string(res))
		return false
	}
}

func (h ApiAAAHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	ctx := r.Context()

	defer panichandler.Http(ctx, w)

	// We always default to application/json
	w.Header().Set("Content-Type", "application/json")

	if handle, params, _ := h.router.Lookup(r.Method, r.URL.Path); handle != nil {
		handle(w, r, params)

		// TODO change me and wrap actions into something that handles server errors
		return 0, nil
	} else {
		if h.HandleAAA(w, r) {
			return h.Next.ServeHTTP(w, r)
		} else {
			// TODO change me and wrap actions into something that handles server errors
			return 0, nil
		}
	}

}
