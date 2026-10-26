# syntax=docker/dockerfile:1.7
# Dashboard SPA validation targets for docker-bake.hcl. Run from the repo root:
#   docker --context <remote> buildx bake dashboard-test dashboard-typecheck dashboard-lint
#   VITEST_ARGS=src/attention docker --context <remote> buildx bake dashboard-test

ARG NODE_IMAGE=node:22-bookworm-slim

FROM ${NODE_IMAGE} AS deps
WORKDIR /src/web
COPY internal/api/dashboardspa/web/package.json internal/api/dashboardspa/web/package-lock.json ./
COPY internal/api/dashboardspa/web/shared/package.json shared/
COPY internal/api/dashboardspa/web/frontend/package.json frontend/
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund

FROM deps AS source
COPY internal/api/dashboardspa/web/ ./

FROM source AS test
ARG VITEST_ARGS=
RUN npm run build:shared && cd frontend && npx vitest run ${VITEST_ARGS}

FROM source AS typecheck
RUN npm run typecheck && npm --workspace gas-city-dashboard-frontend run typecheck:test

FROM source AS lint
RUN npm run build:shared && npm run lint && npm run format:check
