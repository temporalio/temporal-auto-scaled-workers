############################# Main targets #############################
# Rebuild binaries (used by Dockerfile).
bins: temporal-managed-workers

# Install all tools, run all possible checks and tests (long but comprehensive).
all: clean bins test

# Delete all build artifacts
clean: clean-bins clean-test-output
########################################################################

.PHONY: bins clean

##### Variables ######

COLOR := "\e[1;36m%s\e[0m\n"
RED :=   "\e[1;31m%s\e[0m\n"

ALL_SRC         := $(shell find . -name "*.go")
ALL_SRC         += go.mod

##### Binaries #####
clean-bins:
	@printf $(COLOR) "Delete old binaries..."
	@rm -f temporal-managed-workers

temporal-managed-workers: $(ALL_SRC)
	@printf $(COLOR) "Build temporal-managed-workers with CGO_ENABLED=$(CGO_ENABLED) for $(GOOS)/$(GOARCH)..."
	CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_TAG_FLAG) -o temporal-managed-workers ./cmd/worker

##### Tests #####
clean-test-output:
	@printf $(COLOR) "Delete test output..."
	@rm -rf $(TEST_OUTPUT_ROOT)
	@go clean -testcache

unit-test: clean-test-output
	@printf $(COLOR) "Run unit tests..."
	@CGO_ENABLED=$(CGO_ENABLED) go test $(UNIT_TEST_DIRS) $(COMPILED_TEST_ARGS) 2>&1 | tee -a test.log
	@! grep -q "^--- FAIL" test.log

test: unit-test

##### Run server #####
start: start-sqlite-file

start-sqlite: temporal-managed-workers
	./temporal-managed-workers --config-file config/development-sqlite.yaml start

start-sqlite-file: temporal-managed-workers
	./temporal-managed-workers --config-file config/development-sqlite-file.yaml start
