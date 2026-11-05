# Go validation on a remote Docker context. Outputs stay in the build cache.
#   docker --context agent-server buildx bake test
#   PKGS="./internal/telemetry/... ./internal/runtime/acp/..." docker --context agent-server buildx bake test lint

variable "PKGS" { default = "./..." }
variable "RUN" { default = "" }
variable "TEST_FLAGS" { default = "" }

target "_common" {
  context    = "."
  dockerfile = "contrib/bake/Dockerfile"
  output     = ["type=cacheonly"]
  args = {
    PKGS       = PKGS
    RUN        = RUN
    TEST_FLAGS = TEST_FLAGS
  }
}

group "default" { targets = ["build", "vet", "test", "lint"] }

target "deps" {
  inherits = ["_common"]
  target   = "deps"
}

target "build" {
  inherits = ["_common"]
  target   = "build"
}

target "vet" {
  inherits = ["_common"]
  target   = "vet"
}

target "test" {
  inherits = ["_common"]
  target   = "test"
}

target "lint" {
  inherits = ["_common"]
  target   = "lint"
}

# Dashboard SPA (internal/api/dashboardspa/web) validation.
#   VITEST_ARGS=src/attention docker --context agent-server buildx bake dashboard
variable "VITEST_ARGS" { default = "" }

group "dashboard" { targets = ["dashboard-test", "dashboard-typecheck", "dashboard-lint"] }

target "_dashboard" {
  context    = "."
  dockerfile = "contrib/bake/dashboard.Dockerfile"
  output     = ["type=cacheonly"]
  args       = { VITEST_ARGS = VITEST_ARGS }
}

target "dashboard-test" {
  inherits = ["_dashboard"]
  target   = "test"
}

target "dashboard-typecheck" {
  inherits = ["_dashboard"]
  target   = "typecheck"
}

target "dashboard-lint" {
  inherits = ["_dashboard"]
  target   = "lint"
}
