package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/awalterschulze/gographviz"
	"github.com/bom-squad/protobom/pkg/sbom"
	imgv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"zombiezen.com/go/nix/nar"
	"zombiezen.com/go/nix/nixbase32"
)

// App represents the application
type App struct {
	Name         string
	Version      string
	AppType      sbom.Purpose
	ResultHash   string
	ResultDigest string
	BinaryHash   string
}

// GetRuntimeClosureGraph returns the runtime closure graph for the project
// TODO: we should look into adding metadata about licenses, homepage into the graph
func GetRuntimeClosureGraph(appName, output string, symlink string) (*App, *gographviz.Graph, error) {
	app, err := GetAppDetails(output, symlink)
	if err != nil {
		return nil, nil, err
	}
	app.Name = appName
	// todo: maybe we should get version from user.
	app.Version = "0.0.0"

	cmd := exec.Command("nix-store", "-q", "--graph", output+symlink)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		return nil, nil, fmt.Errorf("failed with %s", cmd.Stderr)
	}

	graphAst, err := gographviz.ParseString(stdout.String())
	if err != nil {

		return nil, nil, fmt.Errorf("failed to parse graph: %s", err)
	}

	graph := gographviz.NewGraph()
	if err := gographviz.Analyse(graphAst, graph); err != nil {
		return nil, nil, fmt.Errorf("failed to analyse graph: %s", err)
	}

	addNarHashToGraph(graph)

	app.BinaryHash, err = artifactHash(output, symlink)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get artifact hash: %s", err)
	}

	return app, graph, nil
}

func artifactHash(output, symlink string) (string, error) {
	files, err := os.ReadDir(output + symlink)
	if err != nil {
		return "", err
	}

	for _, file := range files {
		if file.Name() == "bin" {
			binName, err := findResultBinary(output, symlink)
			if err != nil {
				return "", err
			}
			hash, err := fileSHA256(output + symlink + "/bin/" + binName)
			if err != nil {
				return "", err
			}
			return hash, nil
		}
		if file.Name() == "manifest.json" {
			fbytes, err := os.ReadFile(output + symlink + "/manifest.json")
			if err != nil {
				return "", err
			}

			manifest := &imgv1.Manifest{}
			err = json.Unmarshal(fbytes, manifest)
			if err != nil {
				return "", err
			}

			return strings.TrimPrefix(manifest.Config.Digest.String(), "sha256:"), nil

		}
	}

	return "", nil
}

func addNarHashToGraph(graph *gographviz.Graph) {
	var wg sync.WaitGroup

	for _, node := range graph.Nodes.Nodes {
		wg.Add(1)

		go func(node *gographviz.Node) {
			defer wg.Done()
			path := CleanNameFromGraph(node.Name)
			hash, err := GetNarHashFromPath("/nix/store/" + path)
			if err != nil {
				return
			}

			node.Attrs["hash"] = hash
			app, err := parseAppDetails("/nix/store/" + path)
			if err != nil {
				return
			}
			node.Attrs["name"] = app.Name
			node.Attrs["version"] = app.Version
		}(node)
	}

	wg.Wait()
	return
}

// GetNarHashFromPath returns the sha256 hash of the nar
func GetNarHashFromPath(path string) (string, error) {
	h := sha256.New()
	err := nar.DumpPath(h, path)
	if err != nil {
		return "", err
	}

	return nixbase32.EncodeToString(h.Sum(nil)), nil
}

func findResultBinary(output string, symlink string) (string, error) {
	files, err := os.ReadDir(output + symlink + "/bin")
	if err != nil {
		return "", err
	}

	for _, file := range files {
		// As of now, I've only encountered result/bin having a single file
		return file.Name(), nil
	}
	return "", fmt.Errorf("no result binary found")
}

// fileSHA256 returns the sha256 hash of the file
func fileSHA256(filePath string) (string, error) {
	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Create a new SHA-256 hash
	hash := sha256.New()

	// Copy the file content to the hash
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	// Get the SHA-256 checksum
	checksum := hash.Sum(nil)

	// Return the checksum as a hex string
	return hex.EncodeToString(checksum), nil
}

// CleanNameFromGraph removes leading and trailing double quotes and escape characters
func CleanNameFromGraph(s string) string {
	// Remove leading and trailing double quotes
	s = strings.Trim(s, "\"")

	// Remove escape characters
	s = strings.Replace(s, "\\\"", "\"", -1)

	return s
}

// GetAppDetails checks if the symlink exists
func GetAppDetails(output string, symlink string) (*App, error) {
	target, err := os.Readlink(output + symlink)
	if err != nil {
		return nil, fmt.Errorf("failed to read symlink: %v", err)
	}

	hash, err := GetNarHashFromPath(target)
	if err != nil {
		return nil, fmt.Errorf("failed to get nar hash: %v", err)
	}
	app, err := parseAppDetails(target)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app details: %v", err)
	}
	app.ResultHash = hash

	return app, nil
}

func parseAppDetails(path string) (*App, error) {
	fs, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	purpose := findAppType(fs)

	path = strings.TrimPrefix(path, "/nix/store/")
	parts := strings.Split(path, "-")
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	return &App{
		ResultDigest: parts[0],
		Version:      parts[len(parts)-1],
		AppType:      purpose,
	}, nil
}

func findAppType(fs []fs.DirEntry) sbom.Purpose {
	for _, f := range fs {
		if f.Name() == "bin" {
			return sbom.Purpose_EXECUTABLE
		}

		if f.Name() == "manifest.json" {
			return sbom.Purpose_CONTAINER
		}
	}

	return sbom.Purpose_UNKNOWN_PURPOSE
}
