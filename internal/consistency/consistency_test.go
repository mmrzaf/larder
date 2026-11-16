package consistency

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/metrics"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestMatrix_O04_DocumentationConsistency(t *testing.T) {
	root := repositoryRoot(t)
	t.Run("config keys", func(t *testing.T) {
		source := readFile(t, filepath.Join(root, "internal", "config", "types.go")) +
			readFile(t, filepath.Join(root, "internal", "config", "raw.go"))
		reference := readFile(t, filepath.Join(root, "docs", "05-configuration", "reference.md"))
		keyPattern := regexp.MustCompile(`yaml:"([a-z0-9_]+)(?:,[^"]*)?"`)
		for _, match := range keyPattern.FindAllStringSubmatch(source, -1) {
			key := match[1]
			word := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(key) + `([^A-Za-z0-9_]|$)`)
			if !word.MatchString(reference) {
				t.Fatalf("configuration key %q is missing from docs/05-configuration/reference.md", key)
			}
		}
	})

	t.Run("cli commands", func(t *testing.T) {
		codeCommands := commandLiterals(t, filepath.Join(root, "cmd", "larder", "main.go"))
		docs := readFile(t, filepath.Join(root, "docs", "04-cli", "index.md")) +
			readFile(t, filepath.Join(root, "docs", "04-cli", "serve.md")) +
			readFile(t, filepath.Join(root, "docs", "04-cli", "storage-commands.md")) +
			readFile(t, filepath.Join(root, "docs", "04-cli", "warm.md")) +
			readFile(t, filepath.Join(root, "docs", "04-cli", "seed.md")) +
			readFile(t, filepath.Join(root, "docs", "04-cli", "verify-and-repair.md")) +
			readFile(t, filepath.Join(root, "docs", "04-cli", "admin-api.md"))
		commandPattern := regexp.MustCompile("`larder ([a-z][a-z0-9-]*)")
		documented := make(map[string]bool)
		for _, match := range commandPattern.FindAllStringSubmatch(docs, -1) {
			documented[match[1]] = true
		}
		for command := range codeCommands {
			if !documented[command] {
				t.Fatalf("CLI command %q is not documented", command)
			}
		}
		for command := range documented {
			if !codeCommands[command] {
				t.Fatalf("documented CLI command %q is not registered", command)
			}
		}
	})

	t.Run("cli flags", func(t *testing.T) {
		codeFlags := flagLiterals(t, filepath.Join(root, "cmd", "larder"))
		docs := ""
		entries, err := os.ReadDir(filepath.Join(root, "docs", "04-cli"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".md") {
				docs += readFile(t, filepath.Join(root, "docs", "04-cli", entry.Name()))
			}
		}
		pattern := regexp.MustCompile(`--([a-z][a-z0-9-]+)`)
		documented := make(map[string]bool)
		for _, match := range pattern.FindAllStringSubmatch(docs, -1) {
			documented[match[1]] = true
		}
		external := map[string]bool{
			"only-binary": true, "package-lock-only": true, "hash": true,
			"prefer-offline": true, "fetch-retries": true, "print-uris": true,
		}
		for flag := range codeFlags {
			if len(flag) == 1 {
				continue
			}
			if !documented[flag] {
				t.Fatalf("CLI flag --%s is not documented", flag)
			}
		}
		for flag := range documented {
			if external[flag] || len(flag) == 1 {
				continue
			}
			if !codeFlags[flag] {
				t.Fatalf("documented CLI flag --%s is not registered", flag)
			}
		}
	})

	t.Run("metrics", func(t *testing.T) {
		metricSet, err := metrics.New("test")
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		if err := metricSet.WritePrometheus(&buffer); err != nil {
			t.Fatal(err)
		}
		registered := make(map[string]bool)
		typePattern := regexp.MustCompile(`(?m)^# TYPE ([a-z_]+) `)
		for _, match := range typePattern.FindAllStringSubmatch(buffer.String(), -1) {
			registered[match[1]] = true
		}
		observability := readFile(t, filepath.Join(root, "docs", "02-architecture", "observability.md"))
		documented := make(map[string]bool)
		namePattern := regexp.MustCompile(`larder_[a-z_]+`)
		for _, name := range namePattern.FindAllString(observability, -1) {
			documented[name] = true
		}
		for name := range registered {
			if !documented[name] {
				t.Fatalf("metric %s is not documented", name)
			}
		}
		for name := range documented {
			if !registered[name] {
				t.Fatalf("documented metric %s is not registered", name)
			}
		}
	})

	t.Run("test matrix", func(t *testing.T) {
		matrix := readFile(t, filepath.Join(root, "docs", "07-engineering", "test-matrix.md"))
		idPattern := regexp.MustCompile(`^\| ([A-Z]\d{2})( \(R\))? \|`)
		var source strings.Builder
		for _, directory := range []string{"internal", "cmd"} {
			err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
					return err
				}
				source.WriteString(readFile(t, path))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		e2e := readFile(t, filepath.Join(root, "scripts", "e2e-real.sh")) +
			readFile(t, filepath.Join(root, "scripts", "acceptance.sh"))
		for _, line := range strings.Split(matrix, "\n") {
			match := idPattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			id := match[1]
			if strings.Contains(source.String(), "TestMatrix_"+id+"_") {
				continue
			}
			if match[2] != "" && strings.Contains(e2e, id) {
				continue
			}
			t.Fatalf("test matrix ID %s has no TestMatrix_%s_ test", id, id)
		}
	})

	t.Run("example configuration", func(t *testing.T) {
		documented := filepath.Join(root, "docs", "05-configuration", "example.yaml")
		if _, err := config.Load(documented); err != nil {
			t.Fatalf("example configuration does not validate: %v", err)
		}
	})
}

func commandLiterals(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	commands := make(map[string]bool)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "run" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expression := range clause.List {
				literal, ok := expression.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value := strings.Trim(literal.Value, `"`)
				if regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(value) {
					commands[value] = true
				}
			}
			return true
		})
	}
	return commands
}

func flagLiterals(t *testing.T, directory string) map[string]bool {
	t.Helper()
	flags := make(map[string]bool)
	methods := map[string]int{
		"String": 0, "Bool": 0, "Int": 0, "Int64": 0, "Float64": 0, "Duration": 0, "Uint": 0,
		"StringVar": 1, "BoolVar": 1, "IntVar": 1, "Int64Var": 1, "Float64Var": 1, "DurationVar": 1,
		"Var": 1,
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			position, ok := methods[selector.Sel.Name]
			if !ok || position >= len(call.Args) {
				return true
			}
			literal, ok := call.Args[position].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			flags[strings.Trim(literal.Value, `"`)] = true
			return true
		})
	}
	return flags
}

func TestMatrix_O01_DockerImage(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon is not available")
	}
	root := repositoryRoot(t)
	name := "larder-o01-test"
	buildArgs := []string{"build", "-f", "deploy/Dockerfile", "-t", name}
	// Builders behind an HTTP proxy need it inside the build as well.
	if proxy := os.Getenv("HTTPS_PROXY"); proxy != "" {
		buildArgs = append(buildArgs, "--network=host", "--build-arg", "HTTPS_PROXY="+proxy, "--build-arg", "HTTP_PROXY="+proxy)
	}
	// A restricted network reaches the Go modules through its own proxy, as
	// the deployment pipeline does.
	if proxy := os.Getenv("GOPROXY"); proxy != "" {
		buildArgs = append(buildArgs, "--build-arg", "GOPROXY="+proxy)
	}
	build := func() ([]byte, error) {
		command := exec.Command("docker", append(append([]string{}, buildArgs...), ".")...)
		command.Dir = root
		return command.CombinedOutput()
	}
	var buildOutput []byte
	var buildErr error
	// The build pulls its base images and the Go modules from public services.
	// A registry or module proxy that rate-limits anonymous requests is not a
	// broken Dockerfile, so the build is retried before it is reported.
	for attempt := 1; attempt <= 3; attempt++ {
		buildOutput, buildErr = build()
		if buildErr == nil {
			break
		}
		if attempt < 3 {
			t.Logf("docker build attempt %d failed, retrying: %v", attempt, buildErr)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	if buildErr != nil {
		t.Fatalf("docker build: %v\n%s", buildErr, buildOutput)
	}
	example := filepath.Join(root, "docs", "05-configuration", "example.yaml")
	run := exec.Command("docker", "run", "-d", "--name", name, "-v", example+":/etc/larder/larder.yaml:ro", name)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, output)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()
	deadline := time.Now().Add(60 * time.Second)
	for {
		check := exec.Command("docker", "exec", name, "/larder", "healthcheck")
		if err := check.Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("container did not become ready\n%s", logs)
		}
		time.Sleep(time.Second)
	}
	inspect := exec.Command("docker", "inspect", "--format", "{{.Config.User}}", name)
	output, err := inspect.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != "10001:10001" {
		t.Fatalf("container user = %q", strings.TrimSpace(string(output)))
	}
}

func TestMatrix_O02_ComposeConfig(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	root := repositoryRoot(t)
	command := exec.Command("docker", "compose", "-f", "deploy/compose.yaml", "config")
	command.Dir = root
	command.Env = append(os.Environ(), "LARDER_VERSION=0.0.0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, output)
	}
}

func TestMatrix_O03_OfflineDrill(t *testing.T) {
	root := repositoryRoot(t)
	command := exec.Command("sh", "scripts/offline-drill.sh", "--self-test")
	command.Dir = root
	command.Env = append(os.Environ(), "LARDER_BIN="+filepath.Join(t.TempDir(), "larder"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command = exec.CommandContext(ctx, command.Path, command.Args[1:]...)
	command.Dir = root
	command.Env = append(os.Environ(), "LARDER_BIN="+filepath.Join(t.TempDir(), "larder"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("offline drill: %v\n%s", err, output)
	}
}

func TestMatrix_O05_RepositoryHygiene(t *testing.T) {
	root := repositoryRoot(t)
	command := exec.Command("sh", "scripts/hygiene.sh")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("hygiene: %v\n%s", err, output)
	}
}
