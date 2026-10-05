// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// canaryBedrockAccessKey is the synthetic AWS access key fragment the
// credential canary test asserts is never logged or surfaced in any
// error string or returned Candidate field. AC-S1 / MDISC-15.
const canaryBedrockAccessKey = "AKIATESTCANARY12345"

// injectBedrockMockOutput returns an APIOptions hook that registers a
// short-circuit InitializeMiddleware. If retErr is non-nil it is
// returned in place of the operation's normal output; otherwise the
// supplied output is returned as the InitializeOutput.Result. The
// middleware uses position middleware.Before so it runs ahead of the
// SDK's own ListFoundationModels middleware chain.
func injectBedrockMockOutput(t *testing.T, output *bedrock.ListFoundationModelsOutput, retErr error) func(*middleware.Stack) error {
	t.Helper()
	return func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(
			"TestBedrockMockInitialize",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
				if retErr != nil {
					return middleware.InitializeOutput{}, middleware.Metadata{}, retErr
				}
				return middleware.InitializeOutput{Result: output}, middleware.Metadata{}, nil
			},
		), middleware.Before)
	}
}

// newTestBedrockProvider builds a *bedrockProvider with a *bedrock.Client
// constructed from a synthetic aws.Config wired with static dummy
// credentials and the supplied APIOptions hook. This bypasses
// config.LoadDefaultConfig entirely so tests are deterministic in any
// environment (no IMDS calls, no AWS_PROFILE scanning).
func newTestBedrockProvider(t *testing.T, opts ...func(*middleware.Stack) error) *bedrockProvider {
	t.Helper()
	awsCfg := awsv2.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			"DUMMYAKID", "DUMMYSECRET", "",
		),
		APIOptions: opts,
	}
	return &bedrockProvider{regions: []bedrockRegion{{
		name:   "us-east-1",
		client: bedrock.NewFromConfig(awsCfg),
	}}}
}

// newTestBedrockMultiRegion builds a provider with one client per region,
// each wired with its own APIOptions so a test can answer per region.
func newTestBedrockMultiRegion(t *testing.T, regions []string, opts map[string]func(*middleware.Stack) error) *bedrockProvider {
	t.Helper()
	p := &bedrockProvider{}
	for _, r := range regions {
		awsCfg := awsv2.Config{
			Region:      r,
			Credentials: credentials.NewStaticCredentialsProvider("DUMMYAKID", "DUMMYSECRET", ""),
			APIOptions:  []func(*middleware.Stack) error{opts[r]},
		}
		p.regions = append(p.regions, bedrockRegion{name: r, client: bedrock.NewFromConfig(awsCfg)})
	}
	return p
}

// onDemandModel is an ACTIVE, ON_DEMAND, text-output summary fixture.
func onDemandModel(id string) bedrocktypes.FoundationModelSummary {
	return bedrocktypes.FoundationModelSummary{
		ModelId:                 awsv2.String(id),
		ModelLifecycle:          &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatusActive},
		InferenceTypesSupported: []bedrocktypes.InferenceType{bedrocktypes.InferenceTypeOnDemand},
		OutputModalities:        []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
	}
}

// TestBedrock_MultiRegion_OrderAndRegionParams: regions are listed in spec
// order and every candidate carries the region it was found in. Duplicates
// across regions are returned as-is — the reconciler de-duplicates by child
// name (first wins), so order is the contract here.
func TestBedrock_MultiRegion_OrderAndRegionParams(t *testing.T) {
	p := newTestBedrockMultiRegion(t, []string{"eu-north-1", "eu-west-1"}, map[string]func(*middleware.Stack) error{
		"eu-north-1": injectBedrockMockOutput(t, &bedrock.ListFoundationModelsOutput{ModelSummaries: []bedrocktypes.FoundationModelSummary{
			onDemandModel("zai.glm-5"), onDemandModel("openai.gpt-oss-120b-1:0"),
		}}, nil),
		"eu-west-1": injectBedrockMockOutput(t, &bedrock.ListFoundationModelsOutput{ModelSummaries: []bedrocktypes.FoundationModelSummary{
			onDemandModel("google.gemma-3-27b-it"), onDemandModel("openai.gpt-oss-120b-1:0"),
		}}, nil),
	})
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	want := [][2]string{
		{"zai.glm-5", "eu-north-1"},
		{"openai.gpt-oss-120b-1:0", "eu-north-1"},
		{"google.gemma-3-27b-it", "eu-west-1"},
		{"openai.gpt-oss-120b-1:0", "eu-west-1"},
	}
	if len(cands) != len(want) {
		t.Fatalf("got %d candidates %+v; want %d", len(cands), cands, len(want))
	}
	for i, w := range want {
		if cands[i].ID != w[0] || cands[i].Params["aws_region_name"] != w[1] {
			t.Errorf("cand[%d] = %s@%s; want %s@%s", i, cands[i].ID, cands[i].Params["aws_region_name"], w[0], w[1])
		}
	}
}

// TestBedrock_MultiRegion_OneRegionFails: any failing region fails the
// whole List (D-09 atomic snapshot — the reconciler then neither creates
// nor deletes children), and the error names the region.
func TestBedrock_MultiRegion_OneRegionFails(t *testing.T) {
	p := newTestBedrockMultiRegion(t, []string{"eu-north-1", "eu-west-1"}, map[string]func(*middleware.Stack) error{
		"eu-north-1": injectBedrockMockOutput(t, &bedrock.ListFoundationModelsOutput{ModelSummaries: []bedrocktypes.FoundationModelSummary{
			onDemandModel("zai.glm-5"),
		}}, nil),
		"eu-west-1": injectBedrockMockOutput(t, nil, &smithy.GenericAPIError{Code: "InternalFailure", Fault: smithy.FaultServer}),
	})
	cands, err := p.List(context.Background())
	if err == nil {
		t.Fatalf("want error; got %+v", cands)
	}
	if !strings.Contains(err.Error(), "eu-west-1") {
		t.Errorf("error %q does not name the failing region", err)
	}
}

// TestBedrock_HappyPath_PreFilters_ActiveOnDemandNonEmbedding exercises
// the 3-way pre-filter (autoconfig providers.py:251-254 port). Four
// inputs are injected; only the entry that satisfies ALL three
// conditions (ACTIVE lifecycle + ON_DEMAND inference + non-EMBEDDING
// output) should appear in the returned slice.
func TestBedrock_HappyPath_PreFilters_ActiveOnDemandNonEmbedding(t *testing.T) {
	output := &bedrock.ListFoundationModelsOutput{
		ModelSummaries: []bedrocktypes.FoundationModelSummary{
			// KEEP: active + on-demand + text (non-embedding)
			{
				ModelId:                 awsv2.String("anthropic.claude-3-sonnet-20240229-v1:0"),
				ModelName:               awsv2.String("Claude 3 Sonnet"),
				ModelLifecycle:          &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatusActive},
				InferenceTypesSupported: []bedrocktypes.InferenceType{bedrocktypes.InferenceTypeOnDemand},
				OutputModalities:        []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
			},
			// DROP: LEGACY status
			{
				ModelId:                 awsv2.String("anthropic.claude-v1"),
				ModelName:               awsv2.String("Claude v1 (legacy)"),
				ModelLifecycle:          &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatusLegacy},
				InferenceTypesSupported: []bedrocktypes.InferenceType{bedrocktypes.InferenceTypeOnDemand},
				OutputModalities:        []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
			},
			// DROP: PROVISIONED only (no ON_DEMAND)
			{
				ModelId:                 awsv2.String("amazon.titan-text-large-v1"),
				ModelName:               awsv2.String("Titan Text Large (provisioned)"),
				ModelLifecycle:          &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatusActive},
				InferenceTypesSupported: []bedrocktypes.InferenceType{bedrocktypes.InferenceTypeProvisioned},
				OutputModalities:        []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
			},
			// DROP: EMBEDDING output (the SDK's ByInferenceType doesn't cover this)
			{
				ModelId:                 awsv2.String("amazon.titan-embed-text-v1"),
				ModelName:               awsv2.String("Titan Embeddings"),
				ModelLifecycle:          &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatusActive},
				InferenceTypesSupported: []bedrocktypes.InferenceType{bedrocktypes.InferenceTypeOnDemand},
				OutputModalities:        []bedrocktypes.ModelModality{bedrocktypes.ModelModalityEmbedding},
			},
		},
	}
	p := newTestBedrockProvider(t, injectBedrockMockOutput(t, output, nil))

	if p.Type() != "bedrock" {
		t.Fatalf("Type() = %q; want bedrock", p.Type())
	}
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates; want 1 (only ACTIVE + ON_DEMAND + non-EMBEDDING). Got: %+v", len(cands), cands)
	}
	if cands[0].ID != "anthropic.claude-3-sonnet-20240229-v1:0" {
		t.Errorf("cands[0].ID = %q; want anthropic.claude-3-sonnet-20240229-v1:0", cands[0].ID)
	}
	if cands[0].DisplayName != "Claude 3 Sonnet" {
		t.Errorf("cands[0].DisplayName = %q; want Claude 3 Sonnet", cands[0].DisplayName)
	}
}

// TestBedrock_PreservesRawIDWithColons asserts the colon-bearing raw
// model ID is preserved verbatim — normalization to DNS-1123 happens
// later in name derivation, NOT here (MDISC-10 — spec.params.model
// receives the raw ID verbatim).
func TestBedrock_PreservesRawIDWithColons(t *testing.T) {
	const rawID = "anthropic.claude-3-sonnet-20240229-v1:0"
	output := &bedrock.ListFoundationModelsOutput{
		ModelSummaries: []bedrocktypes.FoundationModelSummary{{
			ModelId:                 awsv2.String(rawID),
			ModelName:               awsv2.String("Claude 3 Sonnet"),
			ModelLifecycle:          &bedrocktypes.FoundationModelLifecycle{Status: bedrocktypes.FoundationModelLifecycleStatusActive},
			InferenceTypesSupported: []bedrocktypes.InferenceType{bedrocktypes.InferenceTypeOnDemand},
			OutputModalities:        []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
		}},
	}
	p := newTestBedrockProvider(t, injectBedrockMockOutput(t, output, nil))
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != rawID {
		t.Fatalf("raw colon-bearing ID NOT preserved: got %+v; want ID=%q", cands, rawID)
	}
	if !strings.Contains(cands[0].ID, ":") {
		t.Fatalf("colon stripped from ID: %q", cands[0].ID)
	}
}

// TestBedrock_AccessDenied_ReturnsProviderAuthError verifies that
// smithy.APIError.ErrorCode == "AccessDeniedException" maps to
// *ProviderAuthError (spec §6.3 line 832).
func TestBedrock_AccessDenied_ReturnsProviderAuthError(t *testing.T) {
	apiErr := &smithy.GenericAPIError{
		Code:    "AccessDeniedException",
		Message: "User is not authorized to perform: bedrock:ListFoundationModels",
		Fault:   smithy.FaultClient,
	}
	p := newTestBedrockProvider(t, injectBedrockMockOutput(t, nil, apiErr))
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("AccessDeniedException: want err; got nil")
	}
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("AccessDeniedException: want *ProviderAuthError; got %T %v", listErr, listErr)
	}
	if target.Provider != "bedrock" {
		t.Errorf("target.Provider = %q; want bedrock", target.Provider)
	}
}

// TestBedrock_InvalidSignature_ReturnsProviderAuthError covers the
// InvalidSignatureException code path.
func TestBedrock_InvalidSignature_ReturnsProviderAuthError(t *testing.T) {
	apiErr := &smithy.GenericAPIError{
		Code:    "InvalidSignatureException",
		Message: "Signature expired",
		Fault:   smithy.FaultClient,
	}
	p := newTestBedrockProvider(t, injectBedrockMockOutput(t, nil, apiErr))
	_, listErr := p.List(context.Background())
	var target *ProviderAuthError
	if !errors.As(listErr, &target) {
		t.Fatalf("InvalidSignatureException: want *ProviderAuthError; got %T", listErr)
	}
}

// TestBedrock_5xx_ReturnsPlainError verifies that a transient server
// fault (e.g. InternalFailure) does NOT classify as auth — it must
// return a plain wrapped error so the reconciler maps to Unreachable.
func TestBedrock_5xx_ReturnsPlainError(t *testing.T) {
	apiErr := &smithy.GenericAPIError{
		Code:    "InternalFailure",
		Message: "An internal server error occurred",
		Fault:   smithy.FaultServer,
	}
	p := newTestBedrockProvider(t, injectBedrockMockOutput(t, nil, apiErr))
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("InternalFailure: want err; got nil")
	}
	var target *ProviderAuthError
	if errors.As(listErr, &target) {
		t.Fatalf("InternalFailure must NOT be *ProviderAuthError; got %v", target)
	}
}

// TestBedrock_MissingRegion_ReturnsConstructorError asserts the
// constructor synchronously rejects empty cfg.Region (Bedrock
// CEL-required).
func TestBedrock_MissingRegion_ReturnsConstructorError(t *testing.T) {
	_, err := newBedrock(context.Background(), ProviderConfig{
		Type: "bedrock",
	})
	if err == nil {
		t.Fatal("empty Region: want err")
	}
	// The error string MUST mention region to be useful in status messages.
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error string %q lacks 'region' hint", err.Error())
	}
}

// TestBedrock_SanitizeAWSError_StripsCredentialSubstrings is the unit
// test for the defense-in-depth sanitizer. The SDK does not normally
// echo credentials in errors, but signing-failure messages may include
// request fragments — the helper strips them before wrapping.
func TestBedrock_SanitizeAWSError_StripsCredentialSubstrings(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{
			name: "AWS_ACCESS_KEY_ID env-form leak",
			in:   "signing failure: AWS_ACCESS_KEY_ID=" + canaryBedrockAccessKey + " not found in credential chain",
		},
		{
			name: "SigV4 Credential= header leak",
			in:   "Authorization: AWS4-HMAC-SHA256 Credential=" + canaryBedrockAccessKey + "/20260515/us-east-1/bedrock/aws4_request, SignedHeaders=host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAWSError(errors.New(tc.in))
			if got == nil {
				t.Fatal("sanitizeAWSError returned nil for non-nil input")
			}
			if strings.Contains(got.Error(), canaryBedrockAccessKey) {
				t.Fatalf("sanitized error still contains canary key: %q", got.Error())
			}
		})
	}
	// nil → nil contract.
	if got := sanitizeAWSError(nil); got != nil {
		t.Fatalf("sanitizeAWSError(nil) = %v; want nil", got)
	}
	// no-match input: helper preserves the original error verbatim.
	orig := errors.New("vanilla error with no credential fragment")
	if got := sanitizeAWSError(orig); got != orig {
		t.Errorf("sanitizeAWSError on clean input should return original error; got %v", got)
	}
}

// TestBedrock_CredentialCanary is the MDISC-15 / AC-S1 enforcer.
// Injects a canary AWS access key into cfg.AWSCreds, triggers an
// AccessDeniedException path that *could* echo credential material if
// the SDK ever decided to format it into the error string, and asserts
// the canary fragment does NOT appear in either:
// - the returned error's .Error string
// - the bufferSink logr capture (any code path that logged)
func TestBedrock_CredentialCanary(t *testing.T) {
	buf, _ := newBufferSinkLogger(t)

	// Build a provider via newBedrock so the credential resolution path
	// (D-05 path 1: explicit Secret) is exercised, then attach the
	// middleware mock to its client by overlaying APIOptions through a
	// new client built atop the same Config. Simpler: just construct
	// directly and verify the canary never escapes through the error
	// surface OR the log buffer.
	apiErr := &smithy.GenericAPIError{
		Code:    "AccessDeniedException",
		Message: "auth failed for AWS_ACCESS_KEY_ID=" + canaryBedrockAccessKey,
		Fault:   smithy.FaultClient,
	}
	awsCfg := awsv2.Config{
		Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			canaryBedrockAccessKey, "canary-secret", "",
		),
		APIOptions: []func(*middleware.Stack) error{
			injectBedrockMockOutput(t, nil, apiErr),
		},
	}
	p := &bedrockProvider{regions: []bedrockRegion{{
		name:   "us-east-1",
		client: bedrock.NewFromConfig(awsCfg),
	}}}
	_, listErr := p.List(context.Background())
	if listErr == nil {
		t.Fatal("canary: want err; got nil")
	}
	// The canary key MUST NOT appear in the returned error string —
	// sanitizeAWSError strips both AWS_ACCESS_KEY_ID= and Credential=
	// forms before wrapping.
	if strings.Contains(listErr.Error(), canaryBedrockAccessKey) {
		t.Fatalf("canary leaked into error string: %s", listErr.Error())
	}
	// The log buffer captures any logr.Info/Error calls the provider
	// might emit. The provider currently emits none, but this assertion
	// is the regression bumper if a future edit adds a log line.
	if strings.Contains(buf.String(), canaryBedrockAccessKey) {
		t.Fatalf("canary leaked into log buffer: %s", buf.String())
	}
}

// bedrockOps answers both Bedrock operations from one middleware and
// records the inputs it saw. profilePages[i] answers NextToken == "" (i=0)
// or NextToken == strconv(i); each page but the last links to the next.
type bedrockOps struct {
	models       *bedrock.ListFoundationModelsOutput
	profilePages []*bedrock.ListInferenceProfilesOutput
	profileErr   error

	modelsIn   []*bedrock.ListFoundationModelsInput
	profilesIn []*bedrock.ListInferenceProfilesInput
}

func (o *bedrockOps) middleware() func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(
			"TestBedrockOps",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
				switch p := in.Parameters.(type) {
				case *bedrock.ListFoundationModelsInput:
					o.modelsIn = append(o.modelsIn, p)
					return middleware.InitializeOutput{Result: o.models}, middleware.Metadata{}, nil
				case *bedrock.ListInferenceProfilesInput:
					o.profilesIn = append(o.profilesIn, p)
					if o.profileErr != nil {
						return middleware.InitializeOutput{}, middleware.Metadata{}, o.profileErr
					}
					i, _ := strconv.Atoi(awsv2.ToString(p.NextToken))
					page := *o.profilePages[i]
					if i+1 < len(o.profilePages) {
						page.NextToken = awsv2.String(strconv.Itoa(i + 1))
					}
					return middleware.InitializeOutput{Result: &page}, middleware.Metadata{}, nil
				}
				return middleware.InitializeOutput{}, middleware.Metadata{}, errors.New("unexpected operation")
			},
		), middleware.Before)
	}
}

func profileOnlyModel(id string) bedrocktypes.FoundationModelSummary {
	m := onDemandModel(id)
	m.InferenceTypesSupported = []bedrocktypes.InferenceType{"INFERENCE_PROFILE"}
	return m
}

func systemProfile(id string, status bedrocktypes.InferenceProfileStatus) bedrocktypes.InferenceProfileSummary {
	return bedrocktypes.InferenceProfileSummary{
		InferenceProfileId: awsv2.String(id),
		Status:             status,
		Type:               bedrocktypes.InferenceProfileTypeSystemDefined,
	}
}

// TestBedrock_InferenceProfiles_Selection: in-region ON_DEMAND beats any
// profile; otherwise the first allowed geography (list order) whose ACTIVE
// profile <geo>.<modelId> exists wins; no match drops the model. ID stays
// the base model ID (stable child name), ModelID carries the profile.
func TestBedrock_InferenceProfiles_Selection(t *testing.T) {
	embed := profileOnlyModel("amazon.titan-embed-text-v2:0")
	embed.OutputModalities = []bedrocktypes.ModelModality{bedrocktypes.ModelModalityEmbedding}
	ops := &bedrockOps{
		models: &bedrock.ListFoundationModelsOutput{ModelSummaries: []bedrocktypes.FoundationModelSummary{
			onDemandModel("openai.gpt-oss-120b-1:0"),            // ON_DEMAND (a profile exists too)
			profileOnlyModel("amazon.nova-pro-v1:0"),            // eu + global → eu (list order)
			profileOnlyModel("openai.gpt-6-sol"),                // global only → global
			profileOnlyModel("meta.llama4-maverick-17b-v1:0"),   // us only → dropped
			profileOnlyModel("mistral.pixtral-large-2502-v1:0"), // eu profile INACTIVE → dropped
			embed, // embedding → dropped
		}},
		profilePages: []*bedrock.ListInferenceProfilesOutput{
			{InferenceProfileSummaries: []bedrocktypes.InferenceProfileSummary{
				systemProfile("eu.openai.gpt-oss-120b-1:0", bedrocktypes.InferenceProfileStatusActive),
				systemProfile("global.amazon.nova-pro-v1:0", bedrocktypes.InferenceProfileStatusActive),
				systemProfile("us.meta.llama4-maverick-17b-v1:0", bedrocktypes.InferenceProfileStatusActive),
			}},
			{InferenceProfileSummaries: []bedrocktypes.InferenceProfileSummary{
				systemProfile("eu.amazon.nova-pro-v1:0", bedrocktypes.InferenceProfileStatusActive),
				systemProfile("global.openai.gpt-6-sol", bedrocktypes.InferenceProfileStatusActive),
				systemProfile("eu.mistral.pixtral-large-2502-v1:0", "LEGACY"),
				systemProfile("eu.amazon.titan-embed-text-v2:0", bedrocktypes.InferenceProfileStatusActive),
			}},
		},
	}
	p := newTestBedrockMultiRegion(t, []string{"eu-west-1"}, map[string]func(*middleware.Stack) error{
		"eu-west-1": ops.middleware(),
	})
	p.profiles = []string{"eu", "global"}

	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	want := [][2]string{ // ID, ModelID
		{"openai.gpt-oss-120b-1:0", ""},
		{"amazon.nova-pro-v1:0", "eu.amazon.nova-pro-v1:0"},
		{"openai.gpt-6-sol", "global.openai.gpt-6-sol"},
	}
	if len(cands) != len(want) {
		t.Fatalf("got %+v; want %v", cands, want)
	}
	for i, w := range want {
		if cands[i].ID != w[0] || cands[i].ModelID != w[1] || cands[i].Params["aws_region_name"] != "eu-west-1" {
			t.Errorf("cand[%d] = %+v; want ID=%s ModelID=%q region=eu-west-1", i, cands[i], w[0], w[1])
		}
	}
	if got := ops.modelsIn[0].ByInferenceType; got != "" {
		t.Errorf("ListFoundationModels ByInferenceType = %q; want unset when profiles are enabled", got)
	}
	if len(ops.profilesIn) != 2 {
		t.Fatalf("ListInferenceProfiles calls = %d; want 2 (paginated)", len(ops.profilesIn))
	}
	if got := ops.profilesIn[0].TypeEquals; got != bedrocktypes.InferenceProfileTypeSystemDefined {
		t.Errorf("TypeEquals = %q; want SYSTEM_DEFINED", got)
	}
}

// TestBedrock_NoInferenceProfiles_Unchanged: without the opt-in the request
// is today's ON_DEMAND listing and inference profiles are never listed.
func TestBedrock_NoInferenceProfiles_Unchanged(t *testing.T) {
	ops := &bedrockOps{models: &bedrock.ListFoundationModelsOutput{ModelSummaries: []bedrocktypes.FoundationModelSummary{
		onDemandModel("openai.gpt-oss-120b-1:0"), profileOnlyModel("amazon.nova-pro-v1:0"),
	}}}
	p := newTestBedrockMultiRegion(t, []string{"eu-west-1"}, map[string]func(*middleware.Stack) error{
		"eu-west-1": ops.middleware(),
	})
	cands, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(cands) != 1 || cands[0].ModelID != "" {
		t.Errorf("got %+v; want only the ON_DEMAND model, no ModelID", cands)
	}
	if got := ops.modelsIn[0].ByInferenceType; got != bedrocktypes.InferenceTypeOnDemand {
		t.Errorf("ByInferenceType = %q; want ON_DEMAND", got)
	}
	if len(ops.profilesIn) != 0 {
		t.Errorf("ListInferenceProfiles called %d times; want 0", len(ops.profilesIn))
	}
}

// TestBedrock_InferenceProfiles_AuthError: an IAM denial on
// ListInferenceProfiles classifies as *ProviderAuthError naming the region.
func TestBedrock_InferenceProfiles_AuthError(t *testing.T) {
	ops := &bedrockOps{
		models:     &bedrock.ListFoundationModelsOutput{},
		profileErr: &smithy.GenericAPIError{Code: "AccessDeniedException", Fault: smithy.FaultClient},
	}
	p := newTestBedrockMultiRegion(t, []string{"eu-west-1"}, map[string]func(*middleware.Stack) error{
		"eu-west-1": ops.middleware(),
	})
	p.profiles = []string{"eu"}
	_, err := p.List(context.Background())
	var authErr *ProviderAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("want *ProviderAuthError; got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "eu-west-1") {
		t.Errorf("error %q does not name the region", err)
	}
}
