# syntax=docker/dockerfile:1.7
# Go validation targets for docker-bake.hcl. Run from the repo root:
#   docker --context <remote> buildx bake test
#   PKGS=./internal/telemetry/... docker --context <remote> buildx bake test lint

ARG GO_IMAGE=golang:1.26.10-bookworm

FROM ${GO_IMAGE} AS base
RUN apt-get update \
  && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libicu-dev git \
  && rm -rf /var/lib/apt/lists/*
ENV GOFLAGS=-mod=readonly CGO_ENABLED=1
WORKDIR /src

FROM base AS deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

FROM deps AS source
COPY . .

FROM source AS build
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
  go build ./...

FROM source AS vet
ARG PKGS=./...
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
  go vet ${PKGS}

FROM source AS test
ARG PKGS=./...
ARG RUN=
ARG TEST_FLAGS=
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
  git config --global user.email bake@example.invalid && git config --global user.name bake \
  && GC_FAST_UNIT=1 go test -count=1 -timeout 20m ${RUN:+-run "$RUN"} ${TEST_FLAGS} ${PKGS}

FROM source AS lint
ARG PKGS=./...
ARG GOLANGCI_LINT_VERSION=2.12.0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/.cache/golangci-lint \
  GOFLAGS= go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v${GOLANGCI_LINT_VERSION} \
  && GOMEMLIMIT=6GiB golangci-lint run ${PKGS} \
  && golangci-lint fmt --diff ${PKGS}
