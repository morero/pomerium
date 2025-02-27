// Package evaluator contains rego evaluators for evaluating authorize policy.
package evaluator

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"

	"github.com/go-jose/go-jose/v3"
	"github.com/open-policy-agent/opa/rego"
	"golang.org/x/sync/errgroup"

	"github.com/pomerium/pomerium/authorize/internal/store"
	"github.com/pomerium/pomerium/config"
	"github.com/pomerium/pomerium/internal/httputil"
	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/telemetry/trace"
	"github.com/pomerium/pomerium/pkg/contextutil"
	"github.com/pomerium/pomerium/pkg/cryptutil"
	"github.com/pomerium/pomerium/pkg/policy/criteria"
)

// Request contains the inputs needed for evaluation.
type Request struct {
	IsInternal bool
	Policy     *config.Policy
	HTTP       RequestHTTP
	Session    RequestSession
}

// RequestHTTP is the HTTP field in the request.
type RequestHTTP struct {
	Method            string                `json:"method"`
	Hostname          string                `json:"hostname"`
	Path              string                `json:"path"`
	URL               string                `json:"url"`
	Headers           map[string]string     `json:"headers"`
	ClientCertificate ClientCertificateInfo `json:"client_certificate"`
	IP                string                `json:"ip"`
}

// NewRequestHTTP creates a new RequestHTTP.
func NewRequestHTTP(
	method string,
	requestURL url.URL,
	headers map[string]string,
	clientCertificate ClientCertificateInfo,
	ip string,
) RequestHTTP {
	return RequestHTTP{
		Method:            method,
		Hostname:          requestURL.Hostname(),
		Path:              requestURL.Path,
		URL:               requestURL.String(),
		Headers:           headers,
		ClientCertificate: clientCertificate,
		IP:                ip,
	}
}

// ClientCertificateInfo contains information about the certificate presented
// by the client (if any).
type ClientCertificateInfo struct {
	// Presented is true if the client presented a certificate.
	Presented bool `json:"presented"`

	// Leaf contains the leaf client certificate (unvalidated).
	Leaf string `json:"leaf,omitempty"`

	// Intermediates contains the remainder of the client certificate chain as
	// it was originally presented by the client (unvalidated).
	Intermediates string `json:"intermediates,omitempty"`
}

// RequestSession is the session field in the request.
type RequestSession struct {
	ID string `json:"id"`
}

// Result is the result of evaluation.
type Result struct {
	Allow   RuleResult
	Deny    RuleResult
	Headers http.Header
	Traces  []contextutil.PolicyEvaluationTrace
}

// An Evaluator evaluates policies.
type Evaluator struct {
	store                 *store.Store
	policyEvaluators      map[uint64]*PolicyEvaluator
	headersEvaluators     *HeadersEvaluator
	clientCA              []byte
	clientCRL             []byte
	clientCertConstraints ClientCertConstraints
}

// New creates a new Evaluator.
func New(ctx context.Context, store *store.Store, options ...Option) (*Evaluator, error) {
	e := &Evaluator{store: store}

	cfg := getConfig(options...)

	err := e.updateStore(cfg)
	if err != nil {
		return nil, err
	}

	e.headersEvaluators, err = NewHeadersEvaluator(ctx, store)
	if err != nil {
		return nil, err
	}

	e.clientCA = cfg.clientCA
	e.clientCRL = cfg.clientCRL
	e.clientCertConstraints = cfg.clientCertConstraints

	e.policyEvaluators = make(map[uint64]*PolicyEvaluator)
	for i := range cfg.policies {
		configPolicy := cfg.policies[i]
		id, err := configPolicy.RouteID()
		if err != nil {
			return nil, fmt.Errorf("authorize: error computing policy route id: %w", err)
		}
		policyEvaluator, err :=
			NewPolicyEvaluator(ctx, store, &configPolicy, cfg.addDefaultClientCertificateRule)
		if err != nil {
			return nil, err
		}
		e.policyEvaluators[id] = policyEvaluator
	}

	return e, nil
}

// Evaluate evaluates the rego for the given policy and generates the identity headers.
func (e *Evaluator) Evaluate(ctx context.Context, req *Request) (*Result, error) {
	ctx, span := trace.StartSpan(ctx, "authorize.Evaluator.Evaluate")
	defer span.End()

	eg, ctx := errgroup.WithContext(ctx)

	var policyOutput *PolicyResponse
	eg.Go(func() error {
		var err error
		if req.IsInternal {
			policyOutput, err = e.evaluateInternal(ctx, req)
		} else {
			policyOutput, err = e.evaluatePolicy(ctx, req)
		}
		return err
	})

	var headersOutput *HeadersResponse
	eg.Go(func() error {
		var err error
		headersOutput, err = e.evaluateHeaders(ctx, req)
		return err
	})

	err := eg.Wait()
	if err != nil {
		return nil, err
	}

	res := &Result{
		Allow:   policyOutput.Allow,
		Deny:    policyOutput.Deny,
		Headers: headersOutput.Headers,
		Traces:  policyOutput.Traces,
	}
	return res, nil
}

func (e *Evaluator) evaluateInternal(_ context.Context, req *Request) (*PolicyResponse, error) {
	// these endpoints require a logged-in user
	if req.HTTP.Path == "/.pomerium/webauthn" || req.HTTP.Path == "/.pomerium/jwt" {
		if req.Session.ID == "" {
			return &PolicyResponse{
				Allow: NewRuleResult(false, criteria.ReasonUserUnauthenticated),
			}, nil
		}
	}

	return &PolicyResponse{
		Allow: NewRuleResult(true, criteria.ReasonPomeriumRoute),
	}, nil
}

func (e *Evaluator) evaluatePolicy(ctx context.Context, req *Request) (*PolicyResponse, error) {
	if req.Policy == nil {
		return &PolicyResponse{
			Deny: NewRuleResult(true, criteria.ReasonRouteNotFound),
		}, nil
	}

	id, err := req.Policy.RouteID()
	if err != nil {
		return nil, fmt.Errorf("authorize: error computing policy route id: %w", err)
	}

	policyEvaluator, ok := e.policyEvaluators[id]
	if !ok {
		return &PolicyResponse{
			Deny: NewRuleResult(true, criteria.ReasonRouteNotFound),
		}, nil
	}

	clientCA, err := e.getClientCA(req.Policy)
	if err != nil {
		return nil, err
	}

	isValidClientCertificate, err := isValidClientCertificate(
		clientCA, string(e.clientCRL), req.HTTP.ClientCertificate, e.clientCertConstraints)
	if err != nil {
		return nil, fmt.Errorf("authorize: error validating client certificate: %w", err)
	}

	return policyEvaluator.Evaluate(ctx, &PolicyRequest{
		HTTP:                     req.HTTP,
		Session:                  req.Session,
		IsValidClientCertificate: isValidClientCertificate,
	})
}

func (e *Evaluator) evaluateHeaders(ctx context.Context, req *Request) (*HeadersResponse, error) {
	headersReq := NewHeadersRequestFromPolicy(req.Policy, req.HTTP)
	headersReq.Session = req.Session
	res, err := e.headersEvaluators.Evaluate(ctx, headersReq)
	if err != nil {
		return nil, err
	}

	carryOverJWTAssertion(res.Headers, req.HTTP.Headers)

	return res, nil
}

func (e *Evaluator) getClientCA(policy *config.Policy) (string, error) {
	if policy != nil && policy.TLSDownstreamClientCA != "" {
		bs, err := base64.StdEncoding.DecodeString(policy.TLSDownstreamClientCA)
		if err != nil {
			return "", err
		}
		return string(bs), nil
	}

	return string(e.clientCA), nil
}

func (e *Evaluator) updateStore(cfg *evaluatorConfig) error {
	jwk, err := getJWK(cfg)
	if err != nil {
		return fmt.Errorf("authorize: couldn't create signer: %w", err)
	}

	e.store.UpdateGoogleCloudServerlessAuthenticationServiceAccount(
		cfg.googleCloudServerlessAuthenticationServiceAccount,
	)
	e.store.UpdateJWTClaimHeaders(cfg.jwtClaimsHeaders)
	e.store.UpdateRoutePolicies(cfg.policies)
	e.store.UpdateSigningKey(jwk)

	return nil
}

func getJWK(cfg *evaluatorConfig) (*jose.JSONWebKey, error) {
	var decodedCert []byte
	// if we don't have a signing key, generate one
	if len(cfg.signingKey) == 0 {
		key, err := cryptutil.NewSigningKey()
		if err != nil {
			return nil, fmt.Errorf("couldn't generate signing key: %w", err)
		}
		decodedCert, err = cryptutil.EncodePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("bad signing key: %w", err)
		}
	} else {
		decodedCert = cfg.signingKey
	}

	jwk, err := cryptutil.PrivateJWKFromBytes(decodedCert)
	if err != nil {
		return nil, fmt.Errorf("couldn't generate signing key: %w", err)
	}
	log.Info(context.TODO()).Str("Algorithm", jwk.Algorithm).
		Str("KeyID", jwk.KeyID).
		Interface("Public Key", jwk.Public()).
		Msg("authorize: signing key")

	return jwk, nil
}

func safeEval(ctx context.Context, q rego.PreparedEvalQuery, options ...rego.EvalOption) (resultSet rego.ResultSet, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("%v", e)
		}
	}()
	resultSet, err = q.Eval(ctx, options...)
	return resultSet, err
}

// carryOverJWTAssertion copies assertion JWT from request to response
// note that src keys are expected to be http.CanonicalHeaderKey
func carryOverJWTAssertion(dst http.Header, src map[string]string) {
	jwtForKey := httputil.CanonicalHeaderKey(httputil.HeaderPomeriumJWTAssertionFor)
	jwtFor, ok := src[jwtForKey]
	if ok && jwtFor != "" {
		dst.Add(jwtForKey, jwtFor)
		return
	}
	jwtFor, ok = src[httputil.CanonicalHeaderKey(httputil.HeaderPomeriumJWTAssertion)]
	if ok && jwtFor != "" {
		dst.Add(jwtForKey, jwtFor)
	}
}
