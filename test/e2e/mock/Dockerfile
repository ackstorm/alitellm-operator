# Tier 2 mock provider — see spec/tier2-test-env.md §7
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/litellm-mock .

FROM gcr.io/distroless/static-debian12:nonroot
USER 65532:65532
COPY --from=builder /out/litellm-mock /usr/local/bin/litellm-mock
ENTRYPOINT ["/usr/local/bin/litellm-mock"]
