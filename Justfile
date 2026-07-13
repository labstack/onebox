binary := "ob"
bin_dir := env_var_or_default("OB_BIN_DIR", env_var("HOME") + "/.local/bin")
binary_path := bin_dir + "/" + binary
release := env_var_or_default("OB_VERSION", "0.0.1-m0")

# Build the CLI into a user-local directory on PATH.
default: build

# Build the ob binary.
build:
    mkdir -p "{{ bin_dir }}"
    go build -ldflags "-X github.com/labstack/onebox/internal/buildinfo.release={{ release }} -X github.com/labstack/onebox/internal/buildinfo.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o "{{ binary_path }}" ./cmd/ob
    @echo "built {{ binary_path }}"

# Install the ob binary (alias for build).
install: build

# Run the test suite.
test:
    go test ./...

# Run Go's static analyzer.
vet:
    go vet ./...

# Format all Go packages.
fmt:
    go fmt ./...

# Run all non-mutating checks.
check: test vet

# Remove the installed binary.
clean:
    rm -f "{{ binary_path }}"
