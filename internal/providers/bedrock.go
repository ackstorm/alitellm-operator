// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	smithy "github.com/aws/smithy-go"
)

// bedrockProvider holds the resolved per-CR state for one
// ModelDiscovery instance pointing at AWS Bedrock. The struct is built
// fresh by the reconciler each refresh cycle — per D-05 there is no
// per-call caching (the aws.NewCredentialsCache wrapper is per-Client
// and discarded with it, so Secret rotations propagate within one
// reconcile per MDISC-21).
//
// Unlike the HTTP providers (anthropic / gemini / openai / kubeai),
// bedrockProvider does NOT carry an *http.Client field — aws-sdk-go-v2
// owns the internal transport (D-02 explicit exemption for Bedrock).
// providerTypeBedrock is the wire-level discriminator for the
// Bedrock provider — used in Type(), error log strings, and
// *ProviderAuthError.Provider.
const providerTypeBedrock = "bedrock"

type bedrockProvider struct {
	regions  []bedrockRegion // spec order; earlier wins on duplicate names (reconciler-side)
	profiles []string        // allowed inference-profile geographies, preference order; empty = off
}

type bedrockRegion struct {
	name   string
	client *bedrock.Client
}

// errMissingRegion is the constructor-side sentinel for empty cfg.Regions.
// Bedrock CEL-required this; the constructor still
// validates synchronously so failures surface in status.conditions[]
// before any AWS API call would be issued.
var errMissingRegion = errors.New("bedrock: spec.regions is required")

// awsAccessKeyRe and awsCredentialRe are the package-level pre-compiled
// regexes used by sanitizeAWSError. The two forms cover:
//
// - AWS_ACCESS_KEY_ID=<value> — env-style leak that signing-failure
// diagnostic messages may include.
// - Credential=<value>/<date>/<region>/<service>/aws4_request —
// the SigV4 Authorization header form that signing-debug output
// might echo.
//
// Both are conservative: AWS keys are alphanumeric and the
// Authorization header's Credential field is comma-terminated.
var (
	awsAccessKeyRe  = regexp.MustCompile(`AWS_ACCESS_KEY_ID=[A-Za-z0-9]+`)
	awsCredentialRe = regexp.MustCompile(`Credential=[^,\s]+`)
)

// awsAuthErrorCodes is the closed set of smithy.APIError ErrorCode
// values that classify as auth failures per spec §6.3 line 832. The
// reconciler maps *ProviderAuthError to SourceReachable=False,
// reason=AuthFailed; all other errors map to reason=Unreachable.
//
// The five codes mirror the AWS-published failure modes for
// SigV4-signed control-plane calls:
//
//	AccessDeniedException — IAM policy denies bedrock:ListFoundationModels
//	InvalidSignatureException — clock skew or wrong secret key
//	ExpiredTokenException — STS session credentials expired
//	UnrecognizedClientException — access key not recognized by IAM
//	UnauthorizedException — generic 401 from the service
var awsAuthErrorCodes = map[string]struct{}{
	"AccessDeniedException":       {},
	"InvalidSignatureException":   {},
	"ExpiredTokenException":       {},
	"UnrecognizedClientException": {},
	"UnauthorizedException":       {},
}

// newBedrock is the real constructor — it replaces the 04-03a stub in
// registry.go. The two-path credential resolution implements D-05
// verbatim:
//
// - cfg.AWSCreds != nil → static provider + cache wrap, passed to
// LoadDefaultConfig via WithCredentialsProvider.
// - cfg.AWSCreds == nil → LoadDefaultConfig with region only, picks
// up the default chain (IRSA / pod identity / env / EC2 instance
// profile).
//
// Any error from LoadDefaultConfig is wrapped through sanitizeAWSError
// before surfacing — config errors usually don't carry credentials,
// but the wrap is the canary regression bumper.
func newBedrock(ctx context.Context, cfg ProviderConfig) (Provider, error) {
	if len(cfg.Regions) == 0 {
		return nil, errMissingRegion
	}
	p := &bedrockProvider{profiles: cfg.InferenceProfiles}
	for _, region := range cfg.Regions {
		client, err := newBedrockClient(ctx, cfg.AWSCreds, region)
		if err != nil {
			return nil, err
		}
		p.regions = append(p.regions, bedrockRegion{name: region, client: client})
	}
	return p, nil
}

// newBedrockClient builds one region's client.
func newBedrockClient(ctx context.Context, creds *awsv2.Credentials, region string) (*bedrock.Client, error) {
	var awsCfg awsv2.Config
	var err error
	if creds != nil {
		// D-05 path 1: explicit Secret. The static provider supplies
		// the resolved Access Key / Secret / Session Token from the
		// user's Secret; aws.NewCredentialsCache memoizes Retrieve
		// calls (the SDK's signing middleware calls Retrieve per
		// request; the cache short-circuits to the same struct value
		// for the lifetime of this Client).
		provider := credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID,
			creds.SecretAccessKey,
			creds.SessionToken,
		)
		awsCfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
			config.WithCredentialsProvider(awsv2.NewCredentialsCache(provider)),
		)
	} else {
		// D-05 path 2: default chain. LoadDefaultConfig walks the
		// SDK's documented resolution order: env vars → shared config
		// file → IMDSv2 → ECS task role → IRSA web-identity token →
		// SSO. The operator pod controls the order via its own env
		// (PROJECT.md leaves the pod's IAM identity to the operator).
		awsCfg, err = config.LoadDefaultConfig(ctx, config.WithRegion(region))
	}
	if err != nil {
		return nil, fmt.Errorf("bedrock: aws config: %w", sanitizeAWSError(err))
	}
	return bedrock.NewFromConfig(awsCfg), nil
}

// Type returns the discriminator literal "bedrock". Used by the
// reconciler for metrics labels only — branching on Type outside
// registry.go is the D-01 anti-pattern.
func (p *bedrockProvider) Type() string { return providerTypeBedrock }

// List lists every region in spec order and concatenates the results.
// Each candidate carries Params[aws_region_name] = the region it was found
// in. Duplicates across regions are kept here: the reconciler de-duplicates
// by child name, first wins.
//
// Any failing region fails the whole List. The reconciler then applies the
// D-09 atomic-snapshot rule (no child created or deleted), so children
// discovered from the healthy regions survive an outage of one region.
func (p *bedrockProvider) List(ctx context.Context) ([]Candidate, error) {
	var candidates []Candidate
	for _, r := range p.regions {
		cands, err := r.list(ctx, p.profiles)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, cands...)
	}
	return candidates, nil
}

// list calls ListFoundationModels and applies the in-Go 3-way pre-filter
// (autoconfig providers.py:251-254): ACTIVE, ON_DEMAND, non-EMBEDDING.
//
// With profiles empty the request filters ByInferenceType=OnDemand
// server-side (today's behaviour). With profiles set it lists every model
// plus the region's SYSTEM_DEFINED inference profiles: an ACTIVE,
// non-embedding model WITHOUT in-region ON_DEMAND support is kept when an
// ACTIVE profile "<geo>.<modelId>" exists for the first matching geography
// in profiles order; its Candidate.ModelID is that profile ID while ID stays
// the base model ID (stable child name). In-region ON_DEMAND always beats a
// profile. LiteLLM's cost lookup strips the geo prefix
// (get_bedrock_base_model), so pricing resolves without base_model.
//
// Returns:
// - *ProviderAuthError on smithy.APIError codes in awsAuthErrorCodes
// - plain wrapped error on all other errors (network, retry-exhausted,
// SDK config faults, deserialization errors)
// - sanitized error strings — sanitizeAWSError strips credential
// fragments before wrapping (§9.1)
func (r bedrockRegion) list(ctx context.Context, profiles []string) ([]Candidate, error) {
	in := &bedrock.ListFoundationModelsInput{}
	if len(profiles) == 0 {
		in.ByInferenceType = bedrocktypes.InferenceTypeOnDemand
	}
	out, err := r.client.ListFoundationModels(ctx, in)
	if err != nil {
		return nil, classifyBedrockError(r.name, err)
	}
	var active map[string]struct{}
	if len(profiles) > 0 {
		if active, err = r.activeProfiles(ctx); err != nil {
			return nil, err
		}
	}

	candidates := make([]Candidate, 0, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		id := awsv2.ToString(m.ModelId)
		c := Candidate{
			ID:          id,
			DisplayName: awsv2.ToString(m.ModelName),
			Params:      map[string]string{"aws_region_name": r.name},
		}
		if !shouldKeep(m) {
			if !isActiveNonEmbedding(m) {
				continue
			}
			for _, geo := range profiles {
				if _, ok := active[geo+"."+id]; ok {
					c.ModelID = geo + "." + id
					break
				}
			}
			if c.ModelID == "" {
				continue
			}
		}
		candidates = append(candidates, c)
	}
	return candidates, nil
}

// activeProfiles returns the IDs of the region's ACTIVE SYSTEM_DEFINED
// inference profiles (all pages).
func (r bedrockRegion) activeProfiles(ctx context.Context) (map[string]struct{}, error) {
	ids := map[string]struct{}{}
	pages := bedrock.NewListInferenceProfilesPaginator(r.client, &bedrock.ListInferenceProfilesInput{
		TypeEquals: bedrocktypes.InferenceProfileTypeSystemDefined,
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, classifyBedrockError(r.name, err)
		}
		for _, p := range page.InferenceProfileSummaries {
			if p.Status == bedrocktypes.InferenceProfileStatusActive {
				ids[awsv2.ToString(p.InferenceProfileId)] = struct{}{}
			}
		}
	}
	return ids, nil
}

// shouldKeep applies the 3-way pre-filter (autoconfig providers.py:251-254):
//
// 1. ModelLifecycle != nil and Status == ACTIVE — drops LEGACY entries.
// 2. ON_DEMAND in InferenceTypesSupported — drops PROVISIONED-only
// entries (the SDK's ByInferenceType param duplicates this
// server-side, but the in-Go check is defense-in-depth for any
// unexpected response shape).
// 3. EMBEDDING NOT in OutputModalities — drops embedding models that
// the SDK's filter can't exclude (CONTEXT.md Claude's Discretion
// item 2).
//
// New fields added by future Bedrock API revisions default to zero
// values which fail the filter — the closed-set predicate stays
// conservative (T-04-03c-T2 accept disposition in the threat model).
func shouldKeep(m bedrocktypes.FoundationModelSummary) bool {
	return isActiveNonEmbedding(m) && slices.Contains(m.InferenceTypesSupported, bedrocktypes.InferenceTypeOnDemand)
}

// isActiveNonEmbedding is shouldKeep without the ON_DEMAND condition: the
// gate for routing a model through an inference profile instead.
func isActiveNonEmbedding(m bedrocktypes.FoundationModelSummary) bool {
	return m.ModelLifecycle != nil &&
		m.ModelLifecycle.Status == bedrocktypes.FoundationModelLifecycleStatusActive &&
		!slices.Contains(m.OutputModalities, bedrocktypes.ModelModalityEmbedding)
}

// classifyBedrockError maps smithy.APIError codes to *ProviderAuthError
// or wrapped plain error per spec §6.3 line 832.
//
// The errors.As traversal unwraps the SDK's OperationError envelope and
// any RetryError wrappers — every AWS error path that surfaces an
// underlying APIError satisfies this assertion. Errors with no
// embedded APIError (transport failures, context cancellation,
// deserialization failures) flow through to the plain-error branch.
func classifyBedrockError(region string, err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if _, isAuth := awsAuthErrorCodes[apiErr.ErrorCode()]; isAuth {
			return &ProviderAuthError{
				Provider: providerTypeBedrock,
				Cause:    fmt.Errorf("region %s: %w", region, sanitizeAWSError(err)),
			}
		}
	}
	return fmt.Errorf("bedrock: region %s: list foundation models: %w", region, sanitizeAWSError(err))
}

// sanitizeAWSError strips credential material from error strings before
// wrapping. The SDK does NOT normally include credentials in errors,
// but signing-failure error messages MAY surface request fragments —
// this helper is the §9.1 defense-in-depth canary's last line.
//
// Behavior:
// - nil input → nil (no allocation, satisfies the canonical nil-error
// contract).
// - no regex match → original err returned verbatim (preserves the
// full error chain for errors.Is / errors.As callers).
// - any match → returns errors.New(<scrubbed-message>). The error
// chain is intentionally cut: re-wrapping with fmt.Errorf("%w", err)
// would re-expose err.Error through the SDK's default chain
// formatting, defeating the strip. The acceptable trade-off is
// documented in the threat model (T-04-03c-I1 mitigate).
//
// The two regexes target the documented leak surfaces from AWS
// signing-failure diagnostics:
// - "AWS_ACCESS_KEY_ID=<value>" — env-form leak in signing-chain
// "credentials not found" messages.
// - "Credential=<value>/<date>/<region>/<service>/aws4_request" —
// SigV4 Authorization header form.
func sanitizeAWSError(err error) error {
	if err == nil {
		return nil
	}
	orig := err.Error()
	scrubbed := awsAccessKeyRe.ReplaceAllString(orig, "AWS_ACCESS_KEY_ID=<redacted>")
	scrubbed = awsCredentialRe.ReplaceAllString(scrubbed, "Credential=<redacted>")
	if scrubbed == orig {
		// No leak fragment present — return the original error so the
		// error chain (errors.Unwrap / errors.Is / errors.As) keeps
		// working for downstream callers.
		return err
	}
	return errors.New(scrubbed)
}
