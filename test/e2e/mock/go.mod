// Mock provider binary lives in its own module so it stays stdlib-only
// (per Tier 2 plan amendment A3) and doesn't pull operator deps into the
// distroless image. Built standalone by test/e2e/mock/Dockerfile; not
// part of the main operator module.

module github.com/ackstorm/alitellm-operator/test/e2e/mock

go 1.23.0
