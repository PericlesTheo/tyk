package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/justinas/alice"

	"github.com/TykTechnologies/tyk/internal/uuid"

	"github.com/TykTechnologies/tyk/test"
	"github.com/TykTechnologies/tyk/user"
)

func createRLSession() *user.SessionState {
	session := user.NewSessionState()
	// essentially non-throttled
	session.Rate = 100.0
	session.Allowance = session.Rate
	session.LastCheck = time.Now().Unix()
	session.Per = 1.0
	session.QuotaRenewalRate = 300 // 5 minutes
	session.QuotaRenews = time.Now().Unix()
	session.QuotaRemaining = 10
	session.QuotaMax = 10
	session.AccessRights = map[string]user.AccessDefinition{"31445455": {APIName: "Tyk Auth Key Test", APIID: "31445455", Versions: []string{"default"}}}
	return session
}

func (ts *Test) getRLOpenChain(spec *APISpec) http.Handler {

	remote, _ := url.Parse(spec.Proxy.TargetURL)
	proxy := ts.Gw.TykNewSingleHostReverseProxy(remote, spec, nil)
	proxyHandler := ProxyHandler(proxy, spec)
	baseMid := BaseMiddleware{Spec: spec, Proxy: proxy, Gw: ts.Gw}
	chain := alice.New(ts.Gw.mwList(
		&IPWhiteListMiddleware{baseMid},
		&IPBlackListMiddleware{BaseMiddleware: baseMid},
		&VersionCheck{BaseMiddleware: baseMid},
		&RateLimitForAPI{BaseMiddleware: baseMid},
	)...).Then(proxyHandler)
	return chain
}

func (ts *Test) getGlobalRLAuthKeyChain(spec *APISpec) http.Handler {

	remote, _ := url.Parse(spec.Proxy.TargetURL)
	proxy := ts.Gw.TykNewSingleHostReverseProxy(remote, spec, nil)
	proxyHandler := ProxyHandler(proxy, spec)
	baseMid := BaseMiddleware{Spec: spec, Proxy: proxy, Gw: ts.Gw}
	chain := alice.New(ts.Gw.mwList(
		&IPWhiteListMiddleware{baseMid},
		&IPBlackListMiddleware{BaseMiddleware: baseMid},
		&AuthKey{baseMid},
		&VersionCheck{BaseMiddleware: baseMid},
		&KeyExpired{baseMid},
		&AccessRightsCheck{baseMid},
		&RateLimitForAPI{BaseMiddleware: baseMid},
		&RateLimitAndQuotaCheck{baseMid},
	)...).Then(proxyHandler)
	return chain
}

func TestRLOpen(t *testing.T) {
	ts := StartTest(nil)
	defer ts.Close()

	spec := ts.Gw.LoadSampleAPI(openRLDefSmall)

	req := TestReq(t, "GET", "/rl_test/", nil)

	ts.Gw.DRLManager.SetCurrentTokenValue(1)
	ts.Gw.DRLManager.RequestTokenValue = 1

	ts.Gw.DoReload()
	chain := ts.getRLOpenChain(spec)
	for a := 0; a <= 10; a++ {
		recorder := httptest.NewRecorder()
		chain.ServeHTTP(recorder, req)
		if a < 3 {
			if recorder.Code != 200 {
				t.Fatalf("Rate limit kicked in too early, after only %v requests", a)
			}
		}

		if a > 7 {
			if recorder.Code != 429 {
				t.Fatalf("Rate limit did not activate, code was: %v", recorder.Code)
			}
		}
	}
}

func requestThrottlingTest(limiter string, testLevel string) func(t *testing.T) {
	return func(t *testing.T) {
		ts := StartTest(nil)
		defer ts.Close()

		globalCfg := ts.Gw.GetConfig()

		switch limiter {
		case "InMemoryRateLimiter":
			ts.Gw.DRLManager.SetCurrentTokenValue(1)
			ts.Gw.DRLManager.RequestTokenValue = 1
		case "SentinelRateLimiter":
			globalCfg.EnableSentinelRateLimiter = true
		case "RedisRollingRateLimiter":
			globalCfg.EnableRedisRollingLimiter = true
		default:
			t.Fatal("There is no such a rate limiter:", limiter)
		}

		ts.Gw.SetConfig(globalCfg)

		var per, rate float64
		var throttleRetryLimit int

		per = 2
		rate = 1
		throttleRetryLimit = 3

		// Toggle request throttling on and off, with different throttle intervals.
		iterations := map[bool][]float64{
			true:  {-1, 0, 1},
			false: {-1, 0, 1},
		}

		for requestThrottlingEnabled, throttleIntervals := range iterations {
			for _, throttleInterval := range throttleIntervals {
				spec := ts.Gw.BuildAndLoadAPI(func(spec *APISpec) {
					spec.Name = "test"
					spec.APIID = "test"
					spec.OrgID = "default"
					spec.UseKeylessAccess = false
					spec.Proxy.ListenPath = "/"
				})[0]

				policyID := ts.CreatePolicy(func(p *user.Policy) {
					p.OrgID = "default"

					p.AccessRights = map[string]user.AccessDefinition{
						spec.APIID: {
							APIName: spec.APIDefinition.Name,
							APIID:   spec.APIID,
						},
					}

					if testLevel == "PolicyLevel" {
						p.Per = per
						p.Rate = rate

						if requestThrottlingEnabled {
							p.ThrottleInterval = throttleInterval
							p.ThrottleRetryLimit = throttleRetryLimit
						}
					} else if testLevel == "APILevel" {
						a := p.AccessRights[spec.APIID]
						a.Limit = user.APILimit{
							Rate: rate,
							Per:  per,
						}

						if requestThrottlingEnabled {
							a.Limit.ThrottleInterval = throttleInterval
							a.Limit.ThrottleRetryLimit = throttleRetryLimit
						}

						p.Partitions.PerAPI = true

						p.AccessRights[spec.APIID] = a
					} else {
						t.Fatal("There is no such a test level:", testLevel)
					}
				})

				_, key := ts.CreateSession(func(s *user.SessionState) {
					s.ApplyPolicies = []string{policyID}
				})

				authHeaders := map[string]string{
					"authorization": key,
				}

				if requestThrottlingEnabled && throttleInterval > 0 {
					ts.Run(t, []test.TestCase{
						{Path: "/", Headers: authHeaders, Code: 200, Delay: 100 * time.Millisecond},
						{Path: "/", Headers: authHeaders, Code: 200},
					}...)
				} else {
					ts.Run(t, []test.TestCase{
						{Path: "/", Headers: authHeaders, Code: 200, Delay: 100 * time.Millisecond},
						{Path: "/", Headers: authHeaders, Code: 429},
					}...)
				}
			}
		}
	}
}

func TestRequestThrottling(t *testing.T) {
	test.Flaky(t) // TODO TT-5236

	t.Run("PolicyLevel", func(t *testing.T) {
		t.Run("InMemoryRateLimiter", requestThrottlingTest("InMemoryRateLimiter", "PolicyLevel"))
		t.Run("SentinelRateLimiter", requestThrottlingTest("SentinelRateLimiter", "PolicyLevel"))
		t.Run("RedisRollingRateLimiter", requestThrottlingTest("RedisRollingRateLimiter", "PolicyLevel"))
	})

	t.Run("APILevel", func(t *testing.T) {
		t.Run("InMemoryRateLimiter", requestThrottlingTest("InMemoryRateLimiter", "APILevel"))
		t.Run("SentinelRateLimiter", requestThrottlingTest("SentinelRateLimiter", "APILevel"))
		t.Run("RedisRollingRateLimiter", requestThrottlingTest("RedisRollingRateLimiter", "APILevel"))
	})
}

func TestRLClosed(t *testing.T) {
	ts := StartTest(nil)
	defer ts.Close()

	spec := ts.Gw.LoadSampleAPI(closedRLDefSmall)

	req := TestReq(t, "GET", "/rl_closed_test/", nil)

	session := createRLSession()
	customToken := uuid.New()

	// AuthKey sessions are stored by {token}
	err := ts.Gw.GlobalSessionManager.UpdateSession(customToken, session, 60, false)
	if err != nil {
		t.Error("could not update session in Session Manager. " + err.Error())
	}
	req.Header.Set("authorization", "Bearer "+customToken)

	ts.Gw.DRLManager.SetCurrentTokenValue(1)
	ts.Gw.DRLManager.RequestTokenValue = 1

	chain := ts.getGlobalRLAuthKeyChain(spec)
	for a := 0; a <= 10; a++ {
		recorder := httptest.NewRecorder()
		chain.ServeHTTP(recorder, req)
		if a < 3 {
			if recorder.Code != 200 {
				t.Fatalf("Rate limit kicked in too early, after only %v requests", a)
			}
		}

		if a > 7 {
			if recorder.Code != 429 {
				t.Fatalf("Rate limit did not activate, code was: %v", recorder.Code)
			}
		}
	}
}

// TestJSVMStagesRequest
// TestProcessRequestLiveQuotaLimit
func TestRLOpenWithReload(t *testing.T) {
	ts := StartTest(nil)
	defer ts.Close()

	spec := ts.Gw.LoadSampleAPI(openRLDefSmall)

	req := TestReq(t, "GET", "/rl_test/", nil)

	ts.Gw.DRLManager.SetCurrentTokenValue(1)
	ts.Gw.DRLManager.RequestTokenValue = 1

	chain := ts.getRLOpenChain(spec)
	for a := 0; a <= 10; a++ {
		recorder := httptest.NewRecorder()
		chain.ServeHTTP(recorder, req)
		if a < 3 {
			if recorder.Code != 200 {
				t.Fatalf("Rate limit (pre change) kicked in too early, after only %v requests", a)
			}
		}

		if a > 7 {
			if recorder.Code != 429 {
				t.Fatalf("Rate limit (pre change) did not activate, code was: %v", recorder.Code)
			}
		}
	}

	// Change rate and emulate a reload
	spec.GlobalRateLimit.Rate = 20
	chain = ts.getRLOpenChain(spec)
	for a := 0; a <= 30; a++ {
		recorder := httptest.NewRecorder()
		chain.ServeHTTP(recorder, req)
		if a < 20 {
			if recorder.Code != 200 {
				t.Fatalf("Rate limit (post change) kicked in too early, after only %v requests", a)
			}
		}

		if a > 23 {
			if recorder.Code != 429 {
				t.Fatalf("Rate limit (post change) did not activate, code was: %v", recorder.Code)
			}
		}
	}
}

const openRLDefSmall = `{
	"api_id": "313232",
	"org_id": "default",
	"auth": {"auth_header_name": "authorization"},
	"use_keyless": true,
	"version_data": {
		"not_versioned": true,
		"versions": {
			"v1": {"name": "v1"}
		}
	},
	"proxy": {
		"listen_path": "/rl_test/",
		"target_url": "` + TestHttpAny + `"
	},
	"global_rate_limit": {
		"rate": 3,
		"per": 1
	}
}`

const closedRLDefSmall = `{
	"api_id": "31445455",
	"org_id": "default",
	"auth": {"auth_header_name": "authorization"},
	"version_data": {
		"not_versioned": true,
		"versions": {
			"v1": {"name": "v1"}
		}
	},
	"proxy": {
		"listen_path": "/rl_closed_test/",
		"target_url": "` + TestHttpAny + `"
	},
	"global_rate_limit": {
		"rate": 3,
		"per": 1
	}
}`
