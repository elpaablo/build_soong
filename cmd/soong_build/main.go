// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"android/soong/android"
	"android/soong/bazel"
	"android/soong/bp2build"
	"android/soong/shared"
	"android/soong/ui/metrics/bp2build_metrics_proto"

	"github.com/google/blueprint/bootstrap"
	"github.com/google/blueprint/deptools"
	"github.com/google/blueprint/metrics"
	androidProtobuf "google.golang.org/protobuf/android"
)

var (
	topDir           string
	outDir           string
	soongOutDir      string
	availableEnvFile string
	usedEnvFile      string

	runGoTests bool

	globFile    string
	globListDir string
	delveListen string
	delvePath   string

	moduleGraphFile     string
	moduleActionsFile   string
	docFile             string
	bazelQueryViewDir   string
	bazelApiBp2buildDir string
	bp2buildMarker      string

	cmdlineArgs bootstrap.Args
)

func init() {
	// Flags that make sense in every mode
	flag.StringVar(&topDir, "top", "", "Top directory of the Android source tree")
	flag.StringVar(&soongOutDir, "soong_out", "", "Soong output directory (usually $TOP/out/soong)")
	flag.StringVar(&availableEnvFile, "available_env", "", "File containing available environment variables")
	flag.StringVar(&usedEnvFile, "used_env", "", "File containing used environment variables")
	flag.StringVar(&globFile, "globFile", "build-globs.ninja", "the Ninja file of globs to output")
	flag.StringVar(&globListDir, "globListDir", "", "the directory containing the glob list files")
	flag.StringVar(&outDir, "out", "", "the ninja builddir directory")
	flag.StringVar(&cmdlineArgs.ModuleListFile, "l", "", "file that lists filepaths to parse")

	// Debug flags
	flag.StringVar(&delveListen, "delve_listen", "", "Delve port to listen on for debugging")
	flag.StringVar(&delvePath, "delve_path", "", "Path to Delve. Only used if --delve_listen is set")
	flag.StringVar(&cmdlineArgs.Cpuprofile, "cpuprofile", "", "write cpu profile to file")
	flag.StringVar(&cmdlineArgs.TraceFile, "trace", "", "write trace to file")
	flag.StringVar(&cmdlineArgs.Memprofile, "memprofile", "", "write memory profile to file")
	flag.BoolVar(&cmdlineArgs.NoGC, "nogc", false, "turn off GC for debugging")

	// Flags representing various modes soong_build can run in
	flag.StringVar(&moduleGraphFile, "module_graph_file", "", "JSON module graph file to output")
	flag.StringVar(&moduleActionsFile, "module_actions_file", "", "JSON file to output inputs/outputs of actions of modules")
	flag.StringVar(&docFile, "soong_docs", "", "build documentation file to output")
	flag.StringVar(&bazelQueryViewDir, "bazel_queryview_dir", "", "path to the bazel queryview directory relative to --top")
	flag.StringVar(&bazelApiBp2buildDir, "bazel_api_bp2build_dir", "", "path to the bazel api_bp2build directory relative to --top")
	flag.StringVar(&bp2buildMarker, "bp2build_marker", "", "If set, run bp2build, touch the specified marker file then exit")
	flag.StringVar(&cmdlineArgs.OutFile, "o", "build.ninja", "the Ninja file to output")
	flag.BoolVar(&cmdlineArgs.EmptyNinjaFile, "empty-ninja-file", false, "write out a 0-byte ninja file")
	flag.BoolVar(&cmdlineArgs.BazelMode, "bazel-mode", false, "use bazel for analysis of certain modules")
	flag.BoolVar(&cmdlineArgs.BazelMode, "bazel-mode-staging", false, "use bazel for analysis of certain near-ready modules")
	flag.BoolVar(&cmdlineArgs.BazelModeDev, "bazel-mode-dev", false, "use bazel for analysis of a large number of modules (less stable)")

	// Flags that probably shouldn't be flags of soong_build but we haven't found
	// the time to remove them yet
	flag.BoolVar(&runGoTests, "t", false, "build and run go tests during bootstrap")

	// Disable deterministic randomization in the protobuf package, so incremental
	// builds with unrelated Soong changes don't trigger large rebuilds (since we
	// write out text protos in command lines, and command line changes trigger
	// rebuilds).
	androidProtobuf.DisableRand()
}

func newNameResolver(config android.Config) *android.NameResolver {
	namespacePathsToExport := make(map[string]bool)

	for _, namespaceName := range config.ExportedNamespaces() {
		namespacePathsToExport[namespaceName] = true
	}

	namespacePathsToExport["."] = true // always export the root namespace

	exportFilter := func(namespace *android.Namespace) bool {
		return namespacePathsToExport[namespace.Path]
	}

	return android.NewNameResolver(exportFilter)
}

func newContext(configuration android.Config) *android.Context {
	ctx := android.NewContext(configuration)
	ctx.Register()
	ctx.SetNameInterface(newNameResolver(configuration))
	ctx.SetAllowMissingDependencies(configuration.AllowMissingDependencies())
	return ctx
}

func newConfig(availableEnv map[string]string) android.Config {
	var buildMode android.SoongBuildMode

	if bp2buildMarker != "" {
		buildMode = android.Bp2build
	} else if bazelQueryViewDir != "" {
		buildMode = android.GenerateQueryView
	} else if bazelApiBp2buildDir != "" {
		buildMode = android.ApiBp2build
	} else if moduleGraphFile != "" {
		buildMode = android.GenerateModuleGraph
	} else if docFile != "" {
		buildMode = android.GenerateDocFile
	} else if cmdlineArgs.BazelModeDev {
		buildMode = android.BazelDevMode
	} else if cmdlineArgs.BazelMode {
		buildMode = android.BazelProdMode
	} else if cmdlineArgs.BazelModeStaging {
		buildMode = android.BazelStagingMode
	} else {
		buildMode = android.AnalysisNoBazel
	}

	configuration, err := android.NewConfig(cmdlineArgs.ModuleListFile, buildMode, runGoTests, outDir, soongOutDir, availableEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
		os.Exit(1)
	}
	return configuration
}

// Bazel-enabled mode. Attaches a mutator to queue Bazel requests, adds a
// BeforePrepareBuildActionsHook to invoke Bazel, and then uses Bazel metadata
// for modules that should be handled by Bazel.
func runMixedModeBuild(configuration android.Config, ctx *android.Context, extraNinjaDeps []string) {
	ctx.EventHandler.Begin("mixed_build")
	defer ctx.EventHandler.End("mixed_build")

	bazelHook := func() error {
		ctx.EventHandler.Begin("bazel")
		defer ctx.EventHandler.End("bazel")
		return configuration.BazelContext.InvokeBazel(configuration)
	}
	ctx.SetBeforePrepareBuildActionsHook(bazelHook)
	ninjaDeps := bootstrap.RunBlueprint(cmdlineArgs, bootstrap.DoEverything, ctx.Context, configuration)
	ninjaDeps = append(ninjaDeps, extraNinjaDeps...)

	bazelPaths, err := readBazelPaths(configuration)
	if err != nil {
		panic("Bazel deps file not found: " + err.Error())
	}
	ninjaDeps = append(ninjaDeps, bazelPaths...)

	globListFiles := writeBuildGlobsNinjaFile(ctx, configuration.SoongOutDir(), configuration)
	ninjaDeps = append(ninjaDeps, globListFiles...)

	writeDepFile(cmdlineArgs.OutFile, *ctx.EventHandler, ninjaDeps)
}

// Run the code-generation phase to convert BazelTargetModules to BUILD files.
func runQueryView(queryviewDir, queryviewMarker string, configuration android.Config, ctx *android.Context) {
	ctx.EventHandler.Begin("queryview")
	defer ctx.EventHandler.End("queryview")
	codegenContext := bp2build.NewCodegenContext(configuration, *ctx, bp2build.QueryView)
	absoluteQueryViewDir := shared.JoinPath(topDir, queryviewDir)
	if err := createBazelWorkspace(codegenContext, absoluteQueryViewDir); err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
		os.Exit(1)
	}

	touch(shared.JoinPath(topDir, queryviewMarker))
}

// Run the code-generation phase to convert API contributions to BUILD files.
// Return marker file for the new synthetic workspace
func runApiBp2build(configuration android.Config, extraNinjaDeps []string) string {
	// Create a new context and register mutators that are only meaningful to API export
	ctx := android.NewContext(configuration)
	ctx.EventHandler.Begin("api_bp2build")
	defer ctx.EventHandler.End("api_bp2build")
	ctx.SetNameInterface(newNameResolver(configuration))
	ctx.RegisterForApiBazelConversion()

	// Register the Android.bp files in the tree
	// Add them to the workspace's .d file
	ctx.SetModuleListFile(cmdlineArgs.ModuleListFile)
	if paths, err := ctx.ListModulePaths("."); err == nil {
		extraNinjaDeps = append(extraNinjaDeps, paths...)
	} else {
		panic(err)
	}

	// Run the loading and analysis phase
	ninjaDeps := bootstrap.RunBlueprint(cmdlineArgs,
		bootstrap.StopBeforePrepareBuildActions,
		ctx.Context,
		configuration)
	ninjaDeps = append(ninjaDeps, extraNinjaDeps...)

	// Add the globbed dependencies
	globs := writeBuildGlobsNinjaFile(ctx, configuration.SoongOutDir(), configuration)
	ninjaDeps = append(ninjaDeps, globs...)

	// Run codegen to generate BUILD files
	codegenContext := bp2build.NewCodegenContext(configuration, *ctx, bp2build.ApiBp2build)
	absoluteApiBp2buildDir := shared.JoinPath(topDir, bazelApiBp2buildDir)
	if err := createBazelWorkspace(codegenContext, absoluteApiBp2buildDir); err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
		os.Exit(1)
	}
	ninjaDeps = append(ninjaDeps, codegenContext.AdditionalNinjaDeps()...)

	// Create soong_injection repository
	soongInjectionFiles := bp2build.CreateSoongInjectionFiles(configuration, bp2build.CodegenMetrics{})
	absoluteSoongInjectionDir := shared.JoinPath(topDir, configuration.SoongOutDir(), bazel.SoongInjectionDirName)
	for _, file := range soongInjectionFiles {
		writeReadOnlyFile(absoluteSoongInjectionDir, file)
	}

	workspace := shared.JoinPath(configuration.SoongOutDir(), "api_bp2build")

	excludes := bazelArtifacts()
	// Exclude all src BUILD files
	excludes = append(excludes, apiBuildFileExcludes()...)

	// Create the symlink forest
	symlinkDeps := bp2build.PlantSymlinkForest(
		configuration,
		topDir,
		workspace,
		bazelApiBp2buildDir,
		".",
		excludes)
	ninjaDeps = append(ninjaDeps, symlinkDeps...)

	workspaceMarkerFile := workspace + ".marker"
	writeDepFile(workspaceMarkerFile, *ctx.EventHandler, ninjaDeps)
	touch(shared.JoinPath(topDir, workspaceMarkerFile))
	return workspaceMarkerFile
}

// With some exceptions, api_bp2build does not have any dependencies on the checked-in BUILD files
// Exclude them from the generated workspace to prevent unrelated errors during the loading phase
func apiBuildFileExcludes() []string {
	ret := make([]string, 0)

	srcs, err := getExistingBazelRelatedFiles(topDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error determining existing Bazel-related files: %s\n", err)
		os.Exit(1)
	}
	for _, src := range srcs {
		if src != "WORKSPACE" &&
			src != "BUILD" &&
			src != "BUILD.bazel" &&
			!strings.HasPrefix(src, "build/bazel") &&
			!strings.HasPrefix(src, "prebuilts/clang") {
			ret = append(ret, src)
		}
	}
	return ret
}

func writeMetrics(configuration android.Config, eventHandler metrics.EventHandler, metricsDir string) {
	if len(metricsDir) < 1 {
		fmt.Fprintf(os.Stderr, "\nMissing required env var for generating soong metrics: LOG_DIR\n")
		os.Exit(1)
	}
	metricsFile := filepath.Join(metricsDir, "soong_build_metrics.pb")
	err := android.WriteMetrics(configuration, eventHandler, metricsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error writing soong_build metrics %s: %s", metricsFile, err)
		os.Exit(1)
	}
}

func writeJsonModuleGraphAndActions(ctx *android.Context, graphPath string, actionsPath string) {
	graphFile, graphErr := os.Create(shared.JoinPath(topDir, graphPath))
	actionsFile, actionsErr := os.Create(shared.JoinPath(topDir, actionsPath))
	if graphErr != nil || actionsErr != nil {
		fmt.Fprintf(os.Stderr, "Graph err: %s, actions err: %s", graphErr, actionsErr)
		os.Exit(1)
	}

	defer graphFile.Close()
	defer actionsFile.Close()
	ctx.Context.PrintJSONGraphAndActions(graphFile, actionsFile)
}

func writeBuildGlobsNinjaFile(ctx *android.Context, buildDir string, config interface{}) []string {
	ctx.EventHandler.Begin("globs_ninja_file")
	defer ctx.EventHandler.End("globs_ninja_file")

	globDir := bootstrap.GlobDirectory(buildDir, globListDir)
	bootstrap.WriteBuildGlobsNinjaFile(&bootstrap.GlobSingleton{
		GlobLister: ctx.Globs,
		GlobFile:   globFile,
		GlobDir:    globDir,
		SrcDir:     ctx.SrcDir(),
	}, config)
	return bootstrap.GlobFileListFiles(globDir)
}

func writeDepFile(outputFile string, eventHandler metrics.EventHandler, ninjaDeps []string) {
	eventHandler.Begin("ninja_deps")
	defer eventHandler.End("ninja_deps")
	depFile := shared.JoinPath(topDir, outputFile+".d")
	err := deptools.WriteDepFile(depFile, outputFile, ninjaDeps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing depfile '%s': %s\n", depFile, err)
		os.Exit(1)
	}
}

// doChosenActivity runs Soong for a specific activity, like bp2build, queryview
// or the actual Soong build for the build.ninja file. Returns the top level
// output file of the specific activity.
func doChosenActivity(ctx *android.Context, configuration android.Config, extraNinjaDeps []string) string {
	if configuration.BuildMode == android.Bp2build {
		// Run the alternate pipeline of bp2build mutators and singleton to convert
		// Blueprint to BUILD files before everything else.
		runBp2Build(configuration, extraNinjaDeps)
		return bp2buildMarker
	} else if configuration.IsMixedBuildsEnabled() {
		runMixedModeBuild(configuration, ctx, extraNinjaDeps)
	} else if configuration.BuildMode == android.ApiBp2build {
		return runApiBp2build(configuration, extraNinjaDeps)
	} else {
		var stopBefore bootstrap.StopBefore
		if configuration.BuildMode == android.GenerateModuleGraph {
			stopBefore = bootstrap.StopBeforeWriteNinja
		} else if configuration.BuildMode == android.GenerateQueryView || configuration.BuildMode == android.GenerateDocFile {
			stopBefore = bootstrap.StopBeforePrepareBuildActions
		} else {
			stopBefore = bootstrap.DoEverything
		}

		ninjaDeps := bootstrap.RunBlueprint(cmdlineArgs, stopBefore, ctx.Context, configuration)
		ninjaDeps = append(ninjaDeps, extraNinjaDeps...)

		globListFiles := writeBuildGlobsNinjaFile(ctx, configuration.SoongOutDir(), configuration)
		ninjaDeps = append(ninjaDeps, globListFiles...)

		// Convert the Soong module graph into Bazel BUILD files.
		if configuration.BuildMode == android.GenerateQueryView {
			queryviewMarkerFile := bazelQueryViewDir + ".marker"
			runQueryView(bazelQueryViewDir, queryviewMarkerFile, configuration, ctx)
			writeDepFile(queryviewMarkerFile, *ctx.EventHandler, ninjaDeps)
			return queryviewMarkerFile
		} else if configuration.BuildMode == android.GenerateModuleGraph {
			writeJsonModuleGraphAndActions(ctx, moduleGraphFile, moduleActionsFile)
			writeDepFile(moduleGraphFile, *ctx.EventHandler, ninjaDeps)
			return moduleGraphFile
		} else if configuration.BuildMode == android.GenerateDocFile {
			// TODO: we could make writeDocs() return the list of documentation files
			// written and add them to the .d file. Then soong_docs would be re-run
			// whenever one is deleted.
			if err := writeDocs(ctx, shared.JoinPath(topDir, docFile)); err != nil {
				fmt.Fprintf(os.Stderr, "error building Soong documentation: %s\n", err)
				os.Exit(1)
			}
			writeDepFile(docFile, *ctx.EventHandler, ninjaDeps)
			return docFile
		} else {
			// The actual output (build.ninja) was written in the RunBlueprint() call
			// above
			writeDepFile(cmdlineArgs.OutFile, *ctx.EventHandler, ninjaDeps)
		}
	}

	return cmdlineArgs.OutFile
}

// soong_ui dumps the available environment variables to
// soong.environment.available . Then soong_build itself is run with an empty
// environment so that the only way environment variables can be accessed is
// using Config, which tracks access to them.

// At the end of the build, a file called soong.environment.used is written
// containing the current value of all used environment variables. The next
// time soong_ui is run, it checks whether any environment variables that was
// used had changed and if so, it deletes soong.environment.used to cause a
// rebuild.
//
// The dependency of build.ninja on soong.environment.used is declared in
// build.ninja.d
func parseAvailableEnv() map[string]string {
	if availableEnvFile == "" {
		fmt.Fprintf(os.Stderr, "--available_env not set\n")
		os.Exit(1)
	}

	result, err := shared.EnvFromFile(shared.JoinPath(topDir, availableEnvFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading available environment file '%s': %s\n", availableEnvFile, err)
		os.Exit(1)
	}

	return result
}

func main() {
	flag.Parse()

	shared.ReexecWithDelveMaybe(delveListen, delvePath)
	android.InitSandbox(topDir)

	availableEnv := parseAvailableEnv()

	configuration := newConfig(availableEnv)
	extraNinjaDeps := []string{
		configuration.ProductVariablesFileName,
		usedEnvFile,
	}

	if configuration.Getenv("ALLOW_MISSING_DEPENDENCIES") == "true" {
		configuration.SetAllowMissingDependencies()
	}

	if shared.IsDebugging() {
		// Add a non-existent file to the dependencies so that soong_build will rerun when the debugger is
		// enabled even if it completed successfully.
		extraNinjaDeps = append(extraNinjaDeps, filepath.Join(configuration.SoongOutDir(), "always_rerun_for_delve"))
	}

	// Bypass configuration.Getenv, as LOG_DIR does not need to be dependency tracked. By definition, it will
	// change between every CI build, so tracking it would require re-running Soong for every build.
	logDir := availableEnv["LOG_DIR"]

	ctx := newContext(configuration)
	ctx.EventHandler.Begin("soong_build")

	finalOutputFile := doChosenActivity(ctx, configuration, extraNinjaDeps)

	ctx.EventHandler.End("soong_build")
	writeMetrics(configuration, *ctx.EventHandler, logDir)

	writeUsedEnvironmentFile(configuration, finalOutputFile)
}

func writeUsedEnvironmentFile(configuration android.Config, finalOutputFile string) {
	if usedEnvFile == "" {
		return
	}

	path := shared.JoinPath(topDir, usedEnvFile)
	data, err := shared.EnvFileContents(configuration.EnvDeps())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error writing used environment file '%s': %s\n", usedEnvFile, err)
		os.Exit(1)
	}

	if preexistingData, err := os.ReadFile(path); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error reading used environment file '%s': %s\n", usedEnvFile, err)
			os.Exit(1)
		}
	} else if bytes.Equal(preexistingData, data) {
		// used environment file is unchanged
		return
	}
	if err = os.WriteFile(path, data, 0666); err != nil {
		fmt.Fprintf(os.Stderr, "error writing used environment file '%s': %s\n", usedEnvFile, err)
		os.Exit(1)
	}
	// Touch the output file so that it's not older than the file we just
	// wrote. We can't write the environment file earlier because one an access
	// new environment variables while writing it.
	touch(shared.JoinPath(topDir, finalOutputFile))
}

func touch(path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error touching '%s': %s\n", path, err)
		os.Exit(1)
	}

	err = f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error touching '%s': %s\n", path, err)
		os.Exit(1)
	}

	currentTime := time.Now().Local()
	err = os.Chtimes(path, currentTime, currentTime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error touching '%s': %s\n", path, err)
		os.Exit(1)
	}
}

// Find BUILD files in the srcDir which are not in the allowlist
// (android.Bp2BuildConversionAllowlist#ShouldKeepExistingBuildFileForDir)
// and return their paths so they can be left out of the Bazel workspace dir (i.e. ignored)
func getPathsToIgnoredBuildFiles(config android.Bp2BuildConversionAllowlist, topDir string, srcDirBazelFiles []string, verbose bool) []string {
	paths := make([]string, 0)

	for _, srcDirBazelFileRelativePath := range srcDirBazelFiles {
		srcDirBazelFileFullPath := shared.JoinPath(topDir, srcDirBazelFileRelativePath)
		fileInfo, err := os.Stat(srcDirBazelFileFullPath)
		if err != nil {
			// Warn about error, but continue trying to check files
			fmt.Fprintf(os.Stderr, "WARNING: Error accessing path '%s', err: %s\n", srcDirBazelFileFullPath, err)
			continue
		}
		if fileInfo.IsDir() {
			// Don't ignore entire directories
			continue
		}
		if fileInfo.Name() != "BUILD" && fileInfo.Name() != "BUILD.bazel" {
			// Don't ignore this file - it is not a build file
			continue
		}
		if config.ShouldKeepExistingBuildFileForDir(filepath.Dir(srcDirBazelFileRelativePath)) {
			// Don't ignore this existing build file
			continue
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "Ignoring existing BUILD file: %s\n", srcDirBazelFileRelativePath)
		}
		paths = append(paths, srcDirBazelFileRelativePath)
	}

	return paths
}

// Returns temporary symlink forest excludes necessary for bazel build //external/... (and bazel build //frameworks/...) to work
func getTemporaryExcludes() []string {
	excludes := make([]string, 0)

	// FIXME: 'autotest_lib' is a symlink back to external/autotest, and this causes an infinite symlink expansion error for Bazel
	excludes = append(excludes, "external/autotest/venv/autotest_lib")
	excludes = append(excludes, "external/autotest/autotest_lib")
	excludes = append(excludes, "external/autotest/client/autotest_lib/client")

	// FIXME: The external/google-fruit/extras/bazel_root/third_party/fruit dir is poison
	// It contains several symlinks back to real source dirs, and those source dirs contain BUILD files we want to ignore
	excludes = append(excludes, "external/google-fruit/extras/bazel_root/third_party/fruit")

	// FIXME: 'frameworks/compile/slang' has a filegroup error due to an escaping issue
	excludes = append(excludes, "frameworks/compile/slang")

	return excludes
}

// Read the bazel.list file that the Soong Finder already dumped earlier (hopefully)
// It contains the locations of BUILD files, BUILD.bazel files, etc. in the source dir
func getExistingBazelRelatedFiles(topDir string) ([]string, error) {
	bazelFinderFile := filepath.Join(filepath.Dir(cmdlineArgs.ModuleListFile), "bazel.list")
	if !filepath.IsAbs(bazelFinderFile) {
		// Assume this was a relative path under topDir
		bazelFinderFile = filepath.Join(topDir, bazelFinderFile)
	}
	data, err := ioutil.ReadFile(bazelFinderFile)
	if err != nil {
		return nil, err
	}
	files := strings.Split(strings.TrimSpace(string(data)), "\n")
	return files, nil
}

func bazelArtifacts() []string {
	return []string{
		"bazel-bin",
		"bazel-genfiles",
		"bazel-out",
		"bazel-testlogs",
		"bazel-" + filepath.Base(topDir),
	}
}

// Run Soong in the bp2build mode. This creates a standalone context that registers
// an alternate pipeline of mutators and singletons specifically for generating
// Bazel BUILD files instead of Ninja files.
func runBp2Build(configuration android.Config, extraNinjaDeps []string) {
	var codegenMetrics bp2build.CodegenMetrics
	eventHandler := metrics.EventHandler{}
	eventHandler.Do("bp2build", func() {

		// Register an alternate set of singletons and mutators for bazel
		// conversion for Bazel conversion.
		bp2buildCtx := android.NewContext(configuration)

		// Propagate "allow misssing dependencies" bit. This is normally set in
		// newContext(), but we create bp2buildCtx without calling that method.
		bp2buildCtx.SetAllowMissingDependencies(configuration.AllowMissingDependencies())
		bp2buildCtx.SetNameInterface(newNameResolver(configuration))
		bp2buildCtx.RegisterForBazelConversion()
		bp2buildCtx.SetModuleListFile(cmdlineArgs.ModuleListFile)

		var ninjaDeps []string
		ninjaDeps = append(ninjaDeps, extraNinjaDeps...)

		// Run the loading and analysis pipeline to prepare the graph of regular
		// Modules parsed from Android.bp files, and the BazelTargetModules mapped
		// from the regular Modules.
		eventHandler.Do("bootstrap", func() {
			blueprintArgs := cmdlineArgs
			bootstrapDeps := bootstrap.RunBlueprint(blueprintArgs, bootstrap.StopBeforePrepareBuildActions, bp2buildCtx.Context, configuration)
			ninjaDeps = append(ninjaDeps, bootstrapDeps...)
		})

		globListFiles := writeBuildGlobsNinjaFile(bp2buildCtx, configuration.SoongOutDir(), configuration)
		ninjaDeps = append(ninjaDeps, globListFiles...)

		// Run the code-generation phase to convert BazelTargetModules to BUILD files
		// and print conversion codegenMetrics to the user.
		codegenContext := bp2build.NewCodegenContext(configuration, *bp2buildCtx, bp2build.Bp2Build)
		eventHandler.Do("codegen", func() {
			codegenMetrics = bp2build.Codegen(codegenContext)
		})

		generatedRoot := shared.JoinPath(configuration.SoongOutDir(), "bp2build")
		workspaceRoot := shared.JoinPath(configuration.SoongOutDir(), "workspace")

		excludes := bazelArtifacts()

		if outDir[0] != '/' {
			excludes = append(excludes, outDir)
		}

		existingBazelRelatedFiles, err := getExistingBazelRelatedFiles(topDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error determining existing Bazel-related files: %s\n", err)
			os.Exit(1)
		}

		pathsToIgnoredBuildFiles := getPathsToIgnoredBuildFiles(configuration.Bp2buildPackageConfig, topDir, existingBazelRelatedFiles, configuration.IsEnvTrue("BP2BUILD_VERBOSE"))
		excludes = append(excludes, pathsToIgnoredBuildFiles...)

		excludes = append(excludes, getTemporaryExcludes()...)

		// PlantSymlinkForest() returns all the directories that were readdir()'ed.
		// Such a directory SHOULD be added to `ninjaDeps` so that a child directory
		// or file created/deleted under it would trigger an update of the symlink
		// forest.
		eventHandler.Do("symlink_forest", func() {
			symlinkForestDeps := bp2build.PlantSymlinkForest(
				configuration, topDir, workspaceRoot, generatedRoot, ".", excludes)
			ninjaDeps = append(ninjaDeps, symlinkForestDeps...)
		})

		ninjaDeps = append(ninjaDeps, codegenContext.AdditionalNinjaDeps()...)

		writeDepFile(bp2buildMarker, eventHandler, ninjaDeps)

		// Create an empty bp2build marker file.
		touch(shared.JoinPath(topDir, bp2buildMarker))
	})

	// Only report metrics when in bp2build mode. The metrics aren't relevant
	// for queryview, since that's a total repo-wide conversion and there's a
	// 1:1 mapping for each module.
	if configuration.IsEnvTrue("BP2BUILD_VERBOSE") {
		codegenMetrics.Print()
	}
	writeBp2BuildMetrics(&codegenMetrics, configuration, eventHandler)
}

// Write Bp2Build metrics into $LOG_DIR
func writeBp2BuildMetrics(codegenMetrics *bp2build.CodegenMetrics,
	configuration android.Config, eventHandler metrics.EventHandler) {
	for _, event := range eventHandler.CompletedEvents() {
		codegenMetrics.Events = append(codegenMetrics.Events,
			&bp2build_metrics_proto.Event{
				Name:      event.Id,
				StartTime: uint64(event.Start.UnixNano()),
				RealTime:  event.RuntimeNanoseconds(),
			})
	}
	metricsDir := configuration.Getenv("LOG_DIR")
	if len(metricsDir) < 1 {
		fmt.Fprintf(os.Stderr, "\nMissing required env var for generating bp2build metrics: LOG_DIR\n")
		os.Exit(1)
	}
	codegenMetrics.Write(metricsDir)
}

func readBazelPaths(configuration android.Config) ([]string, error) {
	depsPath := configuration.Getenv("BAZEL_DEPS_FILE")

	data, err := os.ReadFile(depsPath)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSpace(string(data)), "\n")
	return paths, nil
}
