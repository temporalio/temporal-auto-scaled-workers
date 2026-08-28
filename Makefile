############################# Main targets #############################
# Install all tools, run all possible checks and tests (long but comprehensive).
all: clean test

# Delete all build artifacts
clean: clean-test-output
########################################################################

.PHONY: clean

##### Variables ######

COLOR := "\e[1;36m%s\e[0m\n"
RED :=   "\e[1;31m%s\e[0m\n"

ALL_SRC          := $(shell find . -name "*.go")
ALL_SRC          += go.mod
UNIT_TEST_DIRS   ?= ./...
LINT_DIRS        ?= ./...
MAIN_BRANCH      ?= main
TEST_OUTPUT_ROOT ?= $(CURDIR)/reports

# REPORT=true switches test output to `go test -json` plus a coverage profile,
# written under TEST_OUTPUT_ROOT for CI to pick up.
REPORT                      ?= false
UNIT_REPORT_FLAGS           := $(if $(filter true,$(REPORT)),-v -json -coverprofile=$(TEST_OUTPUT_ROOT)/unit-coverage.out)
UNIT_REPORT_REDIRECT        := $(if $(filter true,$(REPORT)),> $(TEST_OUTPUT_ROOT)/unit-report.json)
INTEGRATION_REPORT_FLAGS    := $(if $(filter true,$(REPORT)),-v -json -coverprofile=$(TEST_OUTPUT_ROOT)/integration-coverage.out)
INTEGRATION_REPORT_REDIRECT := $(if $(filter true,$(REPORT)),> $(TEST_OUTPUT_ROOT)/integration-report.json)

##### Binaries #####

##### Tests #####
clean-test-output:
	@printf $(COLOR) "Delete test output..."
	@rm -rf $(TEST_OUTPUT_ROOT)
	@go clean -testcache

unit-test: clean-test-output
	@printf $(COLOR) "Run unit tests..."
	@mkdir -p $(TEST_OUTPUT_ROOT)
	@CGO_ENABLED=$(CGO_ENABLED) go test $(UNIT_TEST_FLAGS) $(UNIT_REPORT_FLAGS) $(UNIT_TEST_DIRS) $(COMPILED_TEST_ARGS) $(UNIT_REPORT_REDIRECT)

integration-test:
	@printf $(COLOR) "Run integration tests..."
	@mkdir -p $(TEST_OUTPUT_ROOT)
	@CGO_ENABLED=$(CGO_ENABLED) go test -C tests -tags=test_dep -timeout 300s ./integration/... $(INTEGRATION_TEST_FLAGS) $(INTEGRATION_REPORT_FLAGS) $(INTEGRATION_REPORT_REDIRECT)

test: unit-test integration-test

##### Linting / formatting #####
lint:
	@printf $(COLOR) "Run golangci-lint..."
	@golangci-lint run $(LINT_DIRS)

# Lint only changes introduced since this branch diverged from $(MAIN_BRANCH).
lint-branch:
	@printf $(COLOR) "Run golangci-lint on changes since merge-base with $(MAIN_BRANCH)..."
	@golangci-lint run --new-from-rev=$$(git merge-base HEAD $(MAIN_BRANCH)) $(LINT_DIRS)

fmt:
	@printf $(COLOR) "Format with golangci-lint..."
	@golangci-lint fmt $(LINT_DIRS)

# Format only Go files changed since this branch diverged from $(MAIN_BRANCH).
fmt-branch:
	@printf $(COLOR) "Format Go files changed since merge-base with $(MAIN_BRANCH)..."
	@files=$$(git diff --name-only --diff-filter=d $$(git merge-base HEAD $(MAIN_BRANCH)) -- '*.go'); \
	if [ -n "$$files" ]; then golangci-lint fmt $$files; else printf $(COLOR) "No changed Go files."; fi

.PHONY: lint lint-branch fmt fmt-branch

##### Run server #####
start: start-sqlite-file

start-sqlite: temporal-auto-scaled-workers
	./temporal-auto-scaled-workers --config-file config/development-sqlite.yaml start

start-sqlite-file: temporal-auto-scaled-workers
	./temporal-auto-scaled-workers --config-file config/development-sqlite-file.yaml start
