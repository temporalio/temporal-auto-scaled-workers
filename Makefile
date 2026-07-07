############################# Main targets #############################
# Rebuild binaries.
bins: temporal-auto-scaled-workers nexus-subprocess-example

# Install all tools, run all possible checks and tests (long but comprehensive).
all: clean bins test

# Delete all build artifacts
clean: clean-bins clean-test-output
########################################################################

.PHONY: bins clean generate

##### Variables ######

COLOR := "\e[1;36m%s\e[0m\n"
RED :=   "\e[1;31m%s\e[0m\n"

ALL_SRC         := $(shell find . -name "*.go")
ALL_SRC         += go.mod
UNIT_TEST_DIRS  ?= ./...
LINT_DIRS       ?= ./...
MAIN_BRANCH     ?= main
NEXUS_EXAMPLE_ADDRESS    ?= localhost:7233
NEXUS_EXAMPLE_NAMESPACE  ?= default
NEXUS_EXAMPLE_TASK_QUEUE ?= nexus-subprocess-example

##### Binaries #####
clean-bins:
	@printf $(COLOR) "Delete old binaries..."
	@rm -f temporal-auto-scaled-workers nexus-subprocess-example

temporal-auto-scaled-workers: generate $(ALL_SRC)
	@printf $(COLOR) "Build temporal-auto-scaled-workers with CGO_ENABLED=$(CGO_ENABLED) for $(GOOS)/$(GOARCH)..."
	CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_TAG_FLAG) -o temporal-auto-scaled-workers ./cmd/worker

nexus-subprocess-example: generate $(ALL_SRC)
	@printf $(COLOR) "Build nexus-subprocess-example with CGO_ENABLED=$(CGO_ENABLED) for $(GOOS)/$(GOARCH)..."
	CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_TAG_FLAG) -o nexus-subprocess-example ./cmd/nexus-subprocess-example

##### Code generation #####
generate:
	@printf $(COLOR) "Run go generate..."
	@go generate ./...

##### Tests #####
clean-test-output:
	@printf $(COLOR) "Delete test output..."
	@rm -rf $(TEST_OUTPUT_ROOT)
	@go clean -testcache

unit-test: generate clean-test-output
	@printf $(COLOR) "Run unit tests..."
	@CGO_ENABLED=$(CGO_ENABLED) go test $(UNIT_TEST_FLAGS) $(UNIT_TEST_DIRS) $(COMPILED_TEST_ARGS) 2>&1 | tee -a test.log
	@! grep -q "^--- FAIL" test.log

test: unit-test

##### Linting / formatting #####
lint: generate
	@printf $(COLOR) "Run golangci-lint..."
	@golangci-lint run $(LINT_DIRS)

# Lint only changes introduced since this branch diverged from $(MAIN_BRANCH).
lint-branch:
	@printf $(COLOR) "Run golangci-lint on changes since merge-base with $(MAIN_BRANCH)..."
	@golangci-lint run --new-from-rev=$$(git merge-base HEAD $(MAIN_BRANCH)) $(LINT_DIRS)

fmt: generate
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

start-nexus-subprocess-example: nexus-subprocess-example
	./nexus-subprocess-example --address $(NEXUS_EXAMPLE_ADDRESS) --namespace $(NEXUS_EXAMPLE_NAMESPACE) --task-queue $(NEXUS_EXAMPLE_TASK_QUEUE)
