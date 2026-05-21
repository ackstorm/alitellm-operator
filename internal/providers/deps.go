// SPDX-License-Identifier: Apache-2.0

package providers

// deps.go anchors the aws-sdk-go-v2 modular dependencies in go.mod so
// Can consume them without needing a separate go.mod
// touch-up. These imports are blank ( _ ".") because // (this plan) implements only the three HTTP providers (Anthropic,
// Gemini, OpenAI) — bedrock.go (which actually uses the AWS SDK)
// lands in 04-03c.
//
// The deps land here, in this earlier plan, so the ProviderConfig
// type can reference *awsv2.Credentials (see interface.go) before the
// bedrock provider exists. CONTEXT.md D-04 / PATTERNS.md require the
// module list:
//
// - github.com/aws/aws-sdk-go-v2/config — credential resolution
// - github.com/aws/aws-sdk-go-v2/credentials — NewStaticCredentialsProvider
// - github.com/aws/aws-sdk-go-v2/service/bedrock — control-plane (NOT bedrock-runtime)
//
// Package Legitimacy: all three are AWS-published modules under
// github.com/aws/aws-sdk-go-v2/* (first-party AWS GitHub org).
// Semver-stable since v1.0.0 GA (2021). Verified via pkg.go.dev
// listings — see 04-03a-SUMMARY.md "Package Legitimacy Audit".
import (
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/aws/aws-sdk-go-v2/service/bedrock"
)
