package main

import (
	"embed"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/coreos/go-semver/semver"
	"github.com/openshift-knative/hack/pkg/rhel"
	"github.com/openshift-knative/hack/pkg/util"

	"github.com/spf13/pflag"
	"go.uber.org/zap/buffer"
	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift-knative/hack/pkg/project"
	"github.com/openshift-knative/hack/pkg/prowgen"
)

const (
	GenerateDockerfileOption           = "dockerfile"
	GenerateMustGatherDockerfileOption = "must-gather-dockerfile"
	defaultAppFilename                 = "main"
	defaultDockerfileTemplateName      = "default"
	funcUtilDockerfileTemplateName     = "func-util"
	mustGatherDockerfileTemplateName   = "must-gather"
	ocClientArtifactsBaseImage         = "registry.ci.openshift.org/ocp/%s:cli-artifacts"
	// builderImageFmt defines the default pattern for the builder image.
	// At the given places, the Go version from the projects go.mod will be inserted.
	// Keep in mind to also update the tools image in the ImageBuilderDockerfile, when the OCP / RHEL
	// version in the pattern gets updated (line 3 and 10).
	builderImageFmt = "registry.ci.openshift.org/openshift/release:rhel-8-release-golang-%s-openshift-4.17"

	// See https://github.com/containerbuildsystem/cachi2/blob/3c562a5410ddd5f1043e7613b240bb5811682f7f/cachi2/core/package_managers/rpm/main.py#L29
	cachi2DefaultRPMsLockFilePath = "rpms.lock.yaml"
)

//go:embed dockerfile-templates/DefaultDockerfile.template
var DockerfileDefaultTemplate embed.FS

//go:embed dockerfile-templates/FuncUtilDockerfile.template
var DockerfileFuncUtilTemplate embed.FS

//go:embed dockerfile-templates/BuildImageDockerfile.template
var DockerfileBuildImageTemplate embed.FS

//go:embed dockerfile-templates/SourceImageDockerfile.template
var DockerfileSourceImageTemplate embed.FS

//go:embed dockerfile-templates/MustGatherDockerfile.template
var DockerfileMustGatherTemplate embed.FS

//go:embed rpms.lock.yaml
var RPMsLockTemplate embed.FS

func main() {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	var (
		rootDir                      string
		includes                     []string
		excludes                     []string
		generators                   string
		output                       string
		dockerfilesDir               string
		dockerfilesTestDir           string
		dockerfilesBuildDir          string
		dockerfilesSourceDir         string
		projectFilePath              string
		dockerfileImageBuilderFmt    string
		appFileFmt                   string
		registryImageFmt             string
		imagesFromRepositories       []string
		imagesFromRepositoriesURLFmt string
		additionalPackages           []string
		templateName                 string
		rpmsLockFileEnabled          bool
	)

	defaultIncludes := []string{
		"test/test_images.*",
		"cmd.*",
	}
	defaultExcludes := []string{
		".*k8s\\.io.*",
		".*knative.dev/pkg/codegen.*",
	}

	pflag.StringVar(&rootDir, "root-dir", wd, "Root directory to start scanning, default to current working directory")
	pflag.StringArrayVar(&includes, "includes", defaultIncludes, "File or directory regex to include")
	pflag.StringArrayVar(&excludes, "excludes", defaultExcludes, "File or directory regex to exclude")
	pflag.StringVar(&generators, "generators", "", "Generate something supported: [dockerfile]")
	pflag.StringVar(&dockerfilesDir, "dockerfile-dir", "ci-operator/knative-images", "Dockerfiles output directory for project images relative to output flag")
	pflag.StringVar(&dockerfilesBuildDir, "dockerfile-build-dir", "ci-operator/build-image", "Dockerfiles output directory for build image relative to output flag")
	pflag.StringVar(&dockerfilesSourceDir, "dockerfile-source-dir", "ci-operator/source-image", "Dockerfiles output directory for source image relative to output flag")
	pflag.StringVar(&dockerfilesTestDir, "dockerfile-test-dir", "ci-operator/knative-test-images", "Dockerfiles output directory for test images relative to output flag")
	pflag.StringVar(&output, "output", filepath.Join(wd, "openshift"), "Output directory")
	pflag.StringVar(&projectFilePath, "project-file", filepath.Join(wd, "openshift", "project.yaml"), "Project metadata file path")
	pflag.StringVar(&dockerfileImageBuilderFmt, "dockerfile-image-builder-fmt", builderImageFmt, "Dockerfile image builder format")
	pflag.StringVar(&appFileFmt, "app-file-fmt", "/usr/bin/%s", "Target application binary path format")
	pflag.StringVar(&registryImageFmt, "registry-image-fmt", "registry.ci.openshift.org/openshift/%s:%s", "Container registry image format")
	pflag.StringArrayVar(&imagesFromRepositories, "images-from", nil, "Additional image references to be pulled from other midstream repositories matching the tag in project.yaml")
	pflag.StringVar(&imagesFromRepositoriesURLFmt, "images-from-url-format", "https://raw.githubusercontent.com/openshift-knative/%s/%s/openshift/images.yaml", "Additional images to be pulled from other midstream repositories matching the tag in project.yaml")
	pflag.StringArrayVar(&additionalPackages, "additional-packages", nil, "Additional packages to be installed in the image")
	pflag.StringVar(&templateName, "template-name", defaultDockerfileTemplateName, fmt.Sprintf("Dockerfile template name to use. Supported values are [%s, %s]", defaultDockerfileTemplateName, funcUtilDockerfileTemplateName))
	pflag.BoolVar(&rpmsLockFileEnabled, "generate-rpms-lock-file", false, "Enable the creation of the rpms.lock.yaml file")
	pflag.Parse()

	if rootDir == "" {
		log.Fatal("root-dir cannot be empty")
	}

	if err := os.Chdir(rootDir); err != nil {
		log.Fatal("Chdir", err, string(debug.Stack()))
	}

	rootDir, err = os.Getwd()
	if err != nil {
		log.Fatal("Getwd", err, string(debug.Stack()))
	}

	includesRegex := util.MustToRegexp(includes)
	excludesRegex := util.MustToRegexp(excludes)

	mainPackagesPaths := sets.NewString()

	err = filepath.Walk(rootDir, func(path string, info fs.FileInfo, err error) error {
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		path = filepath.Join(".", strings.TrimPrefix(path, rootDir))

		include := true
		if len(includesRegex) > 0 {
			include = false
			for _, r := range includesRegex {
				if r.MatchString(path) {
					include = true
					break
				}
			}
		}
		for _, r := range excludesRegex {
			if r.MatchString(path) {
				include = false
				break
			}
		}

		if !include {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("ReadFile %s failed: %w", path, err)
		}
		ast, err := parser.ParseFile(token.NewFileSet(), path, content, parser.PackageClauseOnly)
		if err != nil {
			return fmt.Errorf("ParseFile failed: %w", err)
		}

		if ast.Name.Name != "main" {
			return nil
		}

		mainPackagesPaths.Insert(filepath.Dir(path))
		return nil
	})
	if err != nil {
		log.Fatal(err, "\n", string(debug.Stack()))
	}

	for _, p := range mainPackagesPaths.List() {
		log.Println("Main package path", p)
	}

	if generators == GenerateDockerfileOption {
		goMod := getGoMod(rootDir)
		goVersion := goMod.Go.Version

		// The builder images are distinguished by golang major.minor, so we ignore the rest of the goVersion
		if strings.Count(goVersion, ".") > 1 {
			goVersion = strings.Join(strings.Split(goVersion, ".")[0:2], ".")
		}

		// Builder image might be provided without formatting '%s' string as plain value
		builderImage := dockerfileImageBuilderFmt
		if strings.Count(dockerfileImageBuilderFmt, "%s") == 1 {
			builderImage = fmt.Sprintf(dockerfileImageBuilderFmt, goVersion)
		}

		goPackageToImageMapping := map[string]string{}

		metadata, err := project.ReadMetadataFile(projectFilePath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Fatal("Failed to read project metadata file: ", err)
			}
			log.Println("File ", projectFilePath, " not found")
			metadata = nil
		}

		d := map[string]interface{}{
			"builder": builderImage,
		}
		saveDockerfile(d, DockerfileBuildImageTemplate, output, dockerfilesBuildDir)
		saveDockerfile(d, DockerfileSourceImageTemplate, output, dockerfilesSourceDir)

		var additionalInstructions []string
		if slices.Contains(additionalPackages, "tzdata") {
			rpmsLockFileEnabled = true
			// https://access.redhat.com/solutions/5616681
			additionalInstructions = append(additionalInstructions, fmt.Sprintf("RUN microdnf update tzdata -y && microdnf reinstall tzdata -y"))
			idx := -1
			for i, p := range additionalPackages {
				if strings.TrimSpace(p) == "tzdata" {
					idx = i
					break
				}
			}
			if idx >= 0 {
				additionalPackages = slices.Delete(additionalPackages, idx, idx+1)
			}
		}
		if len(additionalPackages) > 0 {
			additionalInstructions = append(additionalInstructions, fmt.Sprintf("RUN microdnf install %s", strings.Join(additionalPackages, " ")))
		}

		rpmsLockFileWritten := false
		for _, p := range mainPackagesPaths.List() {
			appFile := fmt.Sprintf(appFileFmt, appFilename(p))
			projectName := strings.TrimPrefix(metadata.Project.ImagePrefix, "knative-")
			var projectWithSep, projectDashCaseWithSep string
			if projectName != "" {
				projectWithSep = capitalize(projectName) + " "
				projectDashCaseWithSep = projectName + "-"
			}

			d := map[string]interface{}{
				"main":                    p,
				"app_file":                appFile,
				"builder":                 builderImage,
				"version":                 metadata.Project.Tag,
				"project":                 projectWithSep,
				"project_dashcase":        projectDashCaseWithSep,
				"component":               capitalize(p),
				"component_dashcase":      dashcase(p),
				"additional_instructions": additionalInstructions,
			}

			var DockerfileTemplate embed.FS
			var rpmsLockTemplate *embed.FS
			if rpmsLockFileEnabled {
				rpmsLockTemplate = &RPMsLockTemplate
			}
			switch templateName {
			case defaultDockerfileTemplateName:
				DockerfileTemplate = DockerfileDefaultTemplate
			case funcUtilDockerfileTemplateName:
				DockerfileTemplate = DockerfileFuncUtilTemplate
				rpmsLockTemplate = &RPMsLockTemplate
			default:
				log.Fatal("Unknown template name: " + templateName)
			}

			t, err := template.ParseFS(DockerfileTemplate, "dockerfile-templates/*.template")
			if err != nil {
				log.Fatal("Failed creating template ", err)
			}

			bf := &buffer.Buffer{}
			if err := t.Execute(bf, d); err != nil {
				log.Fatal("Failed to execute template", err)
			}

			out := filepath.Join(output, dockerfilesDir, filepath.Base(p))
			context := prowgen.ProductionContext
			if strings.Contains(p, "test") {
				context = prowgen.TestContext
				out = filepath.Join(output, dockerfilesTestDir, filepath.Base(p))
			}

			dockerfilePath := saveDockerfile(d, DockerfileTemplate, out, "")

			v, err := prowgen.ProjectDirectoryImageBuildStepConfigurationFuncFromImageInput(
				prowgen.Repository{
					ImagePrefix: metadata.Project.ImagePrefix,
				},
				prowgen.ImageInput{
					Context:        context,
					DockerfilePath: dockerfilePath,
				},
			)()
			if err != nil {
				log.Fatal("Failed to derive image name ", err)
			}
			image := fmt.Sprintf(registryImageFmt, v.To, metadata.Project.Tag)
			if imageEnv := os.Getenv(strings.ToUpper(strings.ReplaceAll(string(v.To), "-", "_"))); imageEnv != "" {
				image = imageEnv
			}
			if strings.HasPrefix(p, "vendor/") {
				goPackageToImageMapping[strings.Replace(p, "vendor/", "", 1)] = image
			} else {
				goPackageToImageMapping[filepath.Join(goMod.Module.Mod.Path, p)] = image
			}

			if rpmsLockTemplate != nil && !rpmsLockFileWritten {
				writeRPMLockFile(rpmsLockTemplate, rootDir)
				rpmsLockFileWritten = true
			}
		}

		if err := getAdditionalImagesFromMatchingRepositories(imagesFromRepositories, metadata, imagesFromRepositoriesURLFmt, goPackageToImageMapping); err != nil {
			log.Fatal(err)
		}

		mapping, err := yaml.Marshal(goPackageToImageMapping)
		if err != nil {
			log.Fatal(err)
		}
		// Write the mapping file between Go packages to resolved images.
		// For example:
		// github.com/openshift-knative/hack/cmd/prowgen: registry.ci.openshift.org/openshift/knative-prowgen:knative-v1.8
		// github.com/openshift-knative/hack/cmd/testselect: registry.ci.openshift.org/openshift/knative-test-testselect:knative-v1.8
		if err := os.WriteFile(filepath.Join(output, "images.yaml"), mapping, fs.ModePerm); err != nil {
			log.Fatal("Write images mapping file ", err)
		}
	} else if generators == GenerateMustGatherDockerfileOption {
		var rpmsLockTemplate *embed.FS
		templateName = mustGatherDockerfileTemplateName
		metadata, err := project.ReadMetadataFile(projectFilePath)
		if err != nil {
			log.Fatal("could not read metadata file", err)
		}

		ocClientArtifactsImage := fmt.Sprintf(ocClientArtifactsBaseImage, metadata.Requirements.OcpVersion.Min)
		projectName := mustGatherDockerfileTemplateName
		projectDashCaseWithSep := projectName + "-"

		ocBinaryName, err := getOCBinaryName(metadata)
		if err != nil {
			log.Fatal("could not get oc binary name", err)
		}
		d := map[string]interface{}{
			"main":             projectName,
			"oc_cli_artifacts": ocClientArtifactsImage,
			"oc_binary_name":   ocBinaryName,
			"version":          metadata.Project.Version,
			"project":          capitalize(projectName),
			"project_dashcase": projectDashCaseWithSep,
		}
		out := filepath.Join(output, dockerfilesDir, filepath.Base(projectName))
		saveDockerfile(d, DockerfileMustGatherTemplate, out, "")
		rpmsLockTemplate = &RPMsLockTemplate
		writeRPMLockFile(rpmsLockTemplate, rootDir)
	}
}

func getOCBinaryName(metadata *project.Metadata) (string, error) {
	// depending on the OCP version, the oc binary has different names in registry.ci.openshift.org/ocp/4.13:cli-artifacts:
	// <4.15 it's simply oc, but for >=4.15 it contains two (one for each rhel version: oc.rhel8 & oc.rhel9)

	ocpVersion := metadata.Requirements.OcpVersion.Min

	parts := strings.SplitN(ocpVersion, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid OCP version: %s", ocpVersion)
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("could not convert OCP minor to int (%q): %w", ocpVersion, err)
	}

	if minor <= 14 {
		return "oc", nil
	} else {
		// use rhel suffix for OCP version >= 4.15

		soVersion := semver.New(metadata.Project.Version)
		rhelVersion, err := rhel.ForSOVersion(soVersion)
		if err != nil {
			return "", fmt.Errorf("could not determine rhel version: %v", err)
		}

		return fmt.Sprintf("oc.rhel%s", rhelVersion), nil
	}
}

func appFilename(importpath string) string {
	base := filepath.Base(importpath)

	// If we fail to determine a good name from the importpath then use a
	// safe default.
	if base == "." || base == string(filepath.Separator) {
		return defaultAppFilename
	}

	return base
}

func dashcase(path string) string {
	dir := cmdSubPath(path)
	dir = strings.ReplaceAll(dir, "/", "-")
	dir = strings.ReplaceAll(dir, "_", "-")
	return strings.ToLower(dir)
}

func capitalize(path string) string {
	dir := cmdSubPath(path)
	dir = strings.ReplaceAll(dir, "/", " ")
	dir = strings.ReplaceAll(dir, "_", " ")
	dir = strings.ReplaceAll(dir, "-", " ")
	return strings.Title(strings.ToLower(dir))
}

func cmdSubPath(path string) string {
	if _, dir, found := strings.Cut(path, "cmd/"); found {
		return dir
	}
	return path
}

func getAdditionalImagesFromMatchingRepositories(repositories []string, metadata *project.Metadata, urlFmt string, mapping map[string]string) error {
	branch := strings.Replace(metadata.Project.Tag, "knative", "release", 1)
	branch = strings.Replace(branch, "nightly", "next", 1)
	for _, r := range repositories {
		images, err := downloadImagesFrom(r, branch, urlFmt)
		if err != nil {
			return err
		}

		for k, v := range images {
			// Only add images that are not present
			if _, ok := mapping[k]; !ok {
				log.Println("Additional image from", r, k, v)
				mapping[k] = v
			}
		}
	}

	return nil
}

func downloadImagesFrom(r string, branch string, urlFmt string) (map[string]string, error) {
	url := fmt.Sprintf(urlFmt, r, branch)
	response, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get images for repository %s from %s: %w", r, url, err)
	}
	defer response.Body.Close()

	if response.StatusCode > 400 {
		return nil, fmt.Errorf("failed to get images for repository %s from %s: status code %d", r, url, response.StatusCode)
	}

	content, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	images := make(map[string]string, 8)
	if err := yaml.Unmarshal(content, images); err != nil {
		return nil, fmt.Errorf("failed to get images for repository %s from %s: %w", r, url, err)
	}
	return images, nil
}

func saveDockerfile(d map[string]interface{}, imageTemplate embed.FS, output string, dir string) string {
	bt, err := template.ParseFS(imageTemplate, "dockerfile-templates/*.template")
	if err != nil {
		log.Fatal("Failed creating template ", err)
	}
	bf := &buffer.Buffer{}
	if err := bt.Execute(bf, d); err != nil {
		log.Fatal("Failed to execute template", err)
	}

	out := filepath.Join(output, dir)
	if err := os.RemoveAll(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatal(err)
	}
	if err := os.MkdirAll(out, fs.ModePerm); err != nil && !errors.Is(err, fs.ErrExist) {
		log.Fatal(err)
	}
	dockerfilePath := filepath.Join(out, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, bf.Bytes(), fs.ModePerm); err != nil {
		log.Fatal("Failed writing file", err)
	}

	return dockerfilePath
}

func getGoMod(rootDir string) *modfile.File {
	goModFile := filepath.Join(rootDir, "go.mod")
	goModContent, err := os.ReadFile(goModFile)
	if err != nil {
		log.Fatal("Failed to read go mod file ", goModFile, "error: ", err)
	}

	gm, err := modfile.Parse(goModFile, goModContent, func(path, version string) (string, error) {
		return version, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	return gm
}

func writeRPMLockFile(rpmsLockTemplate fs.FS, rootDir string) {
	// Parse the template file
	t, err := template.ParseFS(rpmsLockTemplate, "rpms.lock.yaml")
	if err != nil {
		log.Fatal("Failed to create RPM template: ", err)
	}

	// Create a buffer to hold the template execution output
	bf := &buffer.Buffer{}
	if err := t.Execute(bf, nil); err != nil {
		log.Fatal("Failed to execute RPM template: ", err)
	}

	// Define the output file path
	outputPath := filepath.Join(rootDir, cachi2DefaultRPMsLockFilePath)

	// Write the generated content to the output file
	if err := os.WriteFile(outputPath, bf.Bytes(), 0644); err != nil {
		log.Fatal("Failed to write RPM lock file: ", err)
	}
}
