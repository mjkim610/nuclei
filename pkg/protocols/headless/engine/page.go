package engine

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/contextargs"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/generators"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/utils/vardump"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/utils"
	httputil "github.com/projectdiscovery/nuclei/v3/pkg/protocols/utils/http"
	"github.com/projectdiscovery/nuclei/v3/pkg/types"
	errorutil "github.com/projectdiscovery/utils/errors"
	urlutil "github.com/projectdiscovery/utils/url"
)

// Page is a single page in an isolated browser instance
type Page struct {
	ctx                *contextargs.Context
	inputURL           *urlutil.URL
	options            *Options
	page               *rod.Page
	rules              []rule
	instance           *Instance
	hijackRouter       *rod.HijackRouter
	hijackNative       *Hijack
	mutex              *sync.RWMutex
	History            []HistoryData
	InteractshURLs     []string
	payloads           map[string]interface{}
	variables          map[string]interface{}
	lastActionNavigate *Action
}

// HistoryData contains the page request/response pairs
type HistoryData struct {
	RawRequest  string
	RawResponse string
}

// Options contains additional configuration options for the browser instance
type Options struct {
	Timeout       time.Duration
	DisableCookie bool
	Options       *types.Options
}

// Run runs a list of actions by creating a new page in the browser.
func (i *Instance) Run(ctx *contextargs.Context, actions []*Action, payloads map[string]interface{}, options *Options) (ActionData, *Page, error) {
	page, err := i.engine.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, nil, err
	}
	page = page.Timeout(options.Timeout)

	if i.browser.customAgent != "" {
		if userAgentErr := page.SetUserAgent(&proto.NetworkSetUserAgentOverride{UserAgent: i.browser.customAgent}); userAgentErr != nil {
			return nil, nil, userAgentErr
		}
	}

	payloads = generators.MergeMaps(payloads,
		generators.BuildPayloadFromOptions(i.browser.options),
	)

	target := ctx.MetaInput.Input
	input, err := urlutil.Parse(target)
	if err != nil {
		return nil, nil, errorutil.NewWithErr(err).Msgf("could not parse URL %s", target)
	}

	hasTrailingSlash := httputil.HasTrailingSlash(target)
	variables := utils.GenerateVariables(input, hasTrailingSlash, contextargs.GenerateVariables(ctx))
	variables = generators.MergeMaps(variables, payloads)

	if vardump.EnableVarDump {
		gologger.Debug().Msgf("Headless Protocol request variables: %s\n", vardump.DumpVariables(variables))
	}

	createdPage := &Page{
		options:   options,
		page:      page,
		ctx:       ctx,
		instance:  i,
		mutex:     &sync.RWMutex{},
		payloads:  payloads,
		variables: variables,
		inputURL:  input,
	}

	httpclient, err := i.browser.getHTTPClient()
	if err != nil {
		return nil, nil, err
	}

	// in case the page has request/response modification rules - enable global hijacking
	if createdPage.hasModificationRules() || containsModificationActions(actions...) {
		hijackRouter := page.HijackRequests()
		if err := hijackRouter.Add("*", "", createdPage.routingRuleHandler(httpclient)); err != nil {
			return nil, nil, err
		}
		createdPage.hijackRouter = hijackRouter
		go hijackRouter.Run()
	} else {
		hijackRouter := NewHijack(page)
		hijackRouter.SetPattern(&proto.FetchRequestPattern{
			URLPattern:   "*",
			RequestStage: proto.FetchRequestStageResponse,
		})
		createdPage.hijackNative = hijackRouter
		hijackRouterHandler := hijackRouter.Start(createdPage.routingRuleHandlerNative)
		go func() {
			_ = hijackRouterHandler()
		}()
	}

	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Viewport: &proto.PageViewport{
		Scale:  1,
		Width:  float64(1920),
		Height: float64(1080),
	}}); err != nil {
		return nil, nil, err
	}

	if _, err := page.SetExtraHeaders([]string{"Accept-Language", "en, en-GB, en-us;"}); err != nil {
		return nil, nil, err
	}

	// inject cookies
	// each http request is performed via the native go http client
	// we first inject the shared cookies
	URL, err := url.Parse(ctx.MetaInput.Input)
	if err != nil {
		return nil, nil, err
	}

	if !options.DisableCookie {
		if cookies := ctx.CookieJar.Cookies(URL); len(cookies) > 0 {
			var NetworkCookies []*proto.NetworkCookie
			for _, cookie := range cookies {
				networkCookie := &proto.NetworkCookie{
					Name:     cookie.Name,
					Value:    cookie.Value,
					Domain:   cookie.Domain,
					Path:     cookie.Path,
					HTTPOnly: cookie.HttpOnly,
					Secure:   cookie.Secure,
					Expires:  proto.TimeSinceEpoch(cookie.Expires.Unix()),
					SameSite: proto.NetworkCookieSameSite(GetSameSite(cookie)),
					Priority: proto.NetworkCookiePriorityLow,
				}
				NetworkCookies = append(NetworkCookies, networkCookie)
			}
			params := proto.CookiesToParams(NetworkCookies)
			for _, param := range params {
				param.URL = ctx.MetaInput.Input
			}
			err := page.SetCookies(params)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	data, err := createdPage.ExecuteActions(ctx, actions)
	if err != nil {
		return nil, nil, err
	}

	if !options.DisableCookie {
		// at the end of actions pull out updated cookies from the browser and inject them into the shared cookie jar
		if cookies, err := page.Cookies([]string{URL.String()}); !options.DisableCookie && err == nil && len(cookies) > 0 {
			var httpCookies []*http.Cookie
			for _, cookie := range cookies {
				httpCookie := &http.Cookie{
					Name:     cookie.Name,
					Value:    cookie.Value,
					Domain:   cookie.Domain,
					Path:     cookie.Path,
					HttpOnly: cookie.HTTPOnly,
					Secure:   cookie.Secure,
				}
				httpCookies = append(httpCookies, httpCookie)
			}
			ctx.CookieJar.SetCookies(URL, httpCookies)
		}
	}

	// The first item of history data will contain the very first request from the browser
	// we assume it's the one matching the initial URL
	if len(createdPage.History) > 0 {
		firstItem := createdPage.History[0]
		if resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(firstItem.RawResponse)), nil); err == nil {
			data["header"] = utils.HeadersToString(resp.Header)
			data["status_code"] = fmt.Sprint(resp.StatusCode)
			defer func() {
				_ = resp.Body.Close()
			}()
		}
	}

	return data, createdPage, nil
}

// Close closes a browser page
func (p *Page) Close() {
	if p.hijackRouter != nil {
		_ = p.hijackRouter.Stop()
	}
	if p.hijackNative != nil {
		_ = p.hijackNative.Stop()
	}
	_ = p.page.Close()
}

// Page returns the current page for the actions
func (p *Page) Page() *rod.Page {
	return p.page
}

// Browser returns the browser that created the current page
func (p *Page) Browser() *rod.Browser {
	return p.instance.engine
}

// URL returns the URL for the current page.
func (p *Page) URL() string {
	info, err := p.page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

// DumpHistory returns the full page navigation history
func (p *Page) DumpHistory() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	var historyDump strings.Builder
	for _, historyData := range p.History {
		historyDump.WriteString(historyData.RawRequest)
		historyDump.WriteString(historyData.RawResponse)
	}
	return historyDump.String()
}

// addToHistory adds a request/response pair to the page history
func (p *Page) addToHistory(historyData ...HistoryData) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.History = append(p.History, historyData...)
}

func (p *Page) addInteractshURL(URLs ...string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.InteractshURLs = append(p.InteractshURLs, URLs...)
}

func (p *Page) hasModificationRules() bool {
	for _, rule := range p.rules {
		if containsAnyModificationActionType(rule.Action) {
			return true
		}
	}
	return false
}

// updateLastNavigatedURL updates the last navigated URL in the instance's
// request log.
func (p *Page) updateLastNavigatedURL() {
	if p.lastActionNavigate == nil {
		return
	}

	templateURL := p.lastActionNavigate.GetArg("url")
	p.instance.requestLog[templateURL] = p.URL()
}

func containsModificationActions(actions ...*Action) bool {
	for _, action := range actions {
		if containsAnyModificationActionType(action.ActionType.ActionType) {
			return true
		}
	}
	return false
}

func containsAnyModificationActionType(actionTypes ...ActionType) bool {
	for _, actionType := range actionTypes {
		switch actionType {
		case ActionSetMethod:
			return true
		case ActionAddHeader:
			return true
		case ActionSetHeader:
			return true
		case ActionDeleteHeader:
			return true
		case ActionSetBody:
			return true
		}
	}
	return false
}

func GetSameSite(cookie *http.Cookie) string {
	switch cookie.SameSite {
	case http.SameSiteNoneMode:
		return "none"
	case http.SameSiteLaxMode:
		return "lax"
	case http.SameSiteStrictMode:
		return "strict"
	case http.SameSiteDefaultMode:
		fallthrough
	default:
		return ""
	}
}
