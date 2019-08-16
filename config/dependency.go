package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl2/hcl"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
)

type Dependency struct {
	Name                                   string     `hcl:",label"`
	ConfigPath                             string     `hcl:"config_path,attr"`
	DefaultOutputs                         *cty.Value `hcl:"default_outputs,attr"`
	DefaultOutputsAllowedTerraformCommands *[]string  `hcl:"default_outputs_allowed_terraform_commands,attr"`
}

// Decode the dependency blocks from the file, and then retrieve all the outputs from the remote state. Then encode the
// resulting map as a cty.Value object.
// TODO: In the future, consider allowing importing dependency blocks from included config
func decodeAndRetrieveOutputs(
	file *hcl.File,
	filename string,
	terragruntOptions *options.TerragruntOptions,
	extensions EvalContextExtensions,
) (*cty.Value, error) {
	decodedDependency := terragruntDependency{}
	err := decodeHcl(file, filename, &decodedDependency, terragruntOptions, extensions)
	if err != nil {
		return nil, err
	}
	return dependencyBlocksToCtyValue(decodedDependency.Dependencies, terragruntOptions)
}

// Convert the list of parsed Dependency blocks into a list of module dependencies. Each output block should
// become a dependency of the current config, since that module has to be applied before we can read the output.
func dependencyBlocksToModuleDependencies(decodedDependencyBlocks []Dependency) *ModuleDependencies {
	if len(decodedDependencyBlocks) == 0 {
		return nil
	}

	paths := []string{}
	for _, decodedDependencyBlock := range decodedDependencyBlocks {
		configPath := decodedDependencyBlock.ConfigPath
		if util.IsFile(configPath) && filepath.Base(configPath) == DefaultTerragruntConfigPath {
			// dependencies system expects the directory containing the terragrunt.hcl file
			configPath = filepath.Dir(configPath)
		}
		paths = append(paths, configPath)
	}
	return &ModuleDependencies{Paths: paths}
}

// Encode the list of dependency blocks into a single cty.Value object that maps the dependency block name to the
// encoded dependency mapping. The encoded dependency mapping should have the attributes:
// - outputs: The map of outputs of the corresponding terraform module that lives at the target config of the
//            dependency.
// This routine will go through the process of obtaining the outputs using `terragrunt output` from the target config.
func dependencyBlocksToCtyValue(dependencyConfigs []Dependency, terragruntOptions *options.TerragruntOptions) (*cty.Value, error) {
	paths := []string{}
	// dependencyMap is the top level map that maps dependency block names to the encoded version, which includes
	// various attributes for accessing information about the target config (including the module outputs).
	dependencyMap := map[string]cty.Value{}

	for _, dependencyConfig := range dependencyConfigs {
		// Loose struct to hold the attributes of the dependency. This includes:
		// - outputs: The module outputs of the target config
		dependencyEncodingMap := map[string]cty.Value{}

		// Encode the outputs and nest under `outputs` attribute
		paths = append(paths, dependencyConfig.ConfigPath)
		outputVal, err := getTerragruntOutputIfAppliedElseConfiguredDefault(dependencyConfig, terragruntOptions)
		if err != nil {
			return nil, err
		}
		dependencyEncodingMap["outputs"] = *outputVal

		// Once the dependency is encoded into a map, we need to conver to a cty.Value again so that it can be fed to
		// the higher order dependency map.
		dependencyEncodingMapEncoded, err := gocty.ToCtyValue(dependencyEncodingMap, generateTypeFromValuesMap(dependencyEncodingMap))
		if err != nil {
			err = TerragruntOutputListEncodingError{Paths: paths, Err: err}
		}

		// Finally, feed the encoded dependency into the higher order map under the block name
		dependencyMap[dependencyConfig.Name] = dependencyEncodingMapEncoded
	}

	// We need to convert the value map to a single cty.Value at the end so that it can be used in the execution context
	convertedOutput, err := gocty.ToCtyValue(dependencyMap, generateTypeFromValuesMap(dependencyMap))
	if err != nil {
		err = TerragruntOutputListEncodingError{Paths: paths, Err: err}
	}
	return &convertedOutput, errors.WithStackTrace(err)
}

// Returns a cleaned path to the target config (the `terragrunt.hcl` file) encoded in the Dependency, handling relative
// paths correctly. This will automatically append `terragrunt.hcl` to the path if the target path is a directory.
func getCleanedTargetConfigPath(dependencyConfig Dependency, terragruntOptions *options.TerragruntOptions) string {
	cwd := filepath.Dir(terragruntOptions.TerragruntConfigPath)
	targetConfig := dependencyConfig.ConfigPath
	if !filepath.IsAbs(targetConfig) {
		targetConfig = util.JoinPath(cwd, targetConfig)
	}
	if util.IsDir(targetConfig) {
		targetConfig = util.JoinPath(targetConfig, DefaultTerragruntConfigPath)
	}
	return targetConfig
}

// This will attempt to get the outputs from the target terragrunt config if it is applied. If it is not applied, the
// behavior is different depending on the configuration of the dependency:
// - If the dependency block indicates a default_outputs attribute, this will return that.
// - If the dependency block does NOT indicate a default_outputs attribute, this will return an error.
func getTerragruntOutputIfAppliedElseConfiguredDefault(dependencyConfig Dependency, terragruntOptions *options.TerragruntOptions) (*cty.Value, error) {
	isApplied, err := isTerragruntApplied(dependencyConfig, terragruntOptions)
	if err != nil {
		return nil, err
	}

	if isApplied {
		return getTerragruntOutput(dependencyConfig, terragruntOptions)
	}

	// When the module is not applied, check if there is a default outputs value to return. If yes, return that. Else,
	// return error.
	targetConfig := getCleanedTargetConfigPath(dependencyConfig, terragruntOptions)
	currentConfig := terragruntOptions.TerragruntConfigPath
	if shouldReturnDefaultOutputs(dependencyConfig, terragruntOptions) {
		util.Debugf(
			terragruntOptions.Logger,
			"WARNING: config %s is a dependency of %s that hasn't been applied, but default outputs provided and returning those in dependency output.",
			targetConfig,
			currentConfig,
		)
		return dependencyConfig.DefaultOutputs, nil
	}

	err = TerragruntOutputTargetNotApplied{
		targetConfig:  targetConfig,
		currentConfig: currentConfig,
	}
	return nil, err
}

// We should only return default outputs if the default_outputs attribute is set, and if we are running one of the
// allowed commands when `default_outputs_allowed_terraform_commands` is set as well.
func shouldReturnDefaultOutputs(dependencyConfig Dependency, terragruntOptions *options.TerragruntOptions) bool {
	defaultOutputsSet := dependencyConfig.DefaultOutputs != nil
	allowedCommand :=
		dependencyConfig.DefaultOutputsAllowedTerraformCommands == nil ||
			len(*dependencyConfig.DefaultOutputsAllowedTerraformCommands) == 0 ||
			util.ListContainsElement(*dependencyConfig.DefaultOutputsAllowedTerraformCommands, terragruntOptions.TerraformCommand)
	return defaultOutputsSet && allowedCommand
}

// Return whether or not the target config has been applied. Returns false when the target config hasn't been applied,
// and true when it has.
func isTerragruntApplied(dependencyConfig Dependency, terragruntOptions *options.TerragruntOptions) (bool, error) {
	// target config check: make sure the target config exists
	targetConfig := getCleanedTargetConfigPath(dependencyConfig, terragruntOptions)
	if !util.FileExists(targetConfig) {
		return false, errors.WithStackTrace(DependencyConfigNotFound{Path: targetConfig})
	}

	// Check if the target terraform module (managed by terragrunt target config) has been applied.
	// When a module hasn't been applied yet, there will be no state object, so `terragrunt show` will indicate that.
	stdout, err := runTerragruntShow(terragruntOptions, targetConfig)
	stdout = strings.TrimSpace(stdout)
	isApplied := stdout != "No state."
	util.Debugf(
		terragruntOptions.Logger,
		"Dependency %s of %s is applied: %v (output of terragrunt show: %s)",
		targetConfig,
		terragruntOptions.TerragruntConfigPath,
		isApplied,
		stdout,
	)
	return isApplied, err
}

// Return the output from the state of another module, managed by terragrunt. This function will parse the provided
// terragrunt config and extract the desired output from the remote state. Note that this will error if the targetted
// module hasn't been applied yet.
func getTerragruntOutput(dependencyConfig Dependency, terragruntOptions *options.TerragruntOptions) (*cty.Value, error) {
	// target config check: make sure the target config exists
	targetConfig := getCleanedTargetConfigPath(dependencyConfig, terragruntOptions)
	if !util.FileExists(targetConfig) {
		return nil, errors.WithStackTrace(DependencyConfigNotFound{Path: targetConfig})
	}

	jsonBytes, err := runTerragruntOutputJson(terragruntOptions, targetConfig)
	if err != nil {
		return nil, err
	}

	outputMap, err := terraformOutputJsonToCtyValueMap(targetConfig, jsonBytes)
	if err != nil {
		return nil, err
	}

	// We need to convert the value map to a single cty.Value at the end for use in the terragrunt config.
	convertedOutput, err := gocty.ToCtyValue(outputMap, generateTypeFromValuesMap(outputMap))
	if err != nil {
		err = TerragruntOutputEncodingError{Path: targetConfig, Err: err}
	}
	return &convertedOutput, errors.WithStackTrace(err)
}

// runTerragruntShow uses terragrunt running functions to show the current state of the target config.
func runTerragruntShow(terragruntOptions *options.TerragruntOptions, targetConfig string) (string, error) {
	var stdoutBuffer bytes.Buffer
	stdoutBufferWriter := bufio.NewWriter(&stdoutBuffer)
	targetOptions := terragruntOptions.Clone(targetConfig)
	targetOptions.TerraformCliArgs = []string{"show", "-no-color"}
	targetOptions.Writer = stdoutBufferWriter
	err := targetOptions.RunTerragrunt(targetOptions)
	if err != nil {
		return "", errors.WithStackTrace(err)
	}

	stdoutBufferWriter.Flush()
	outputString := stdoutBuffer.String()
	return outputString, nil
}

// runTerragruntOutputJson uses terragrunt running functions to extract the json output from the target config. Make a
// copy of the terragruntOptions so that we can reuse the same execution environment.
func runTerragruntOutputJson(terragruntOptions *options.TerragruntOptions, targetConfig string) ([]byte, error) {
	var stdoutBuffer bytes.Buffer
	stdoutBufferWriter := bufio.NewWriter(&stdoutBuffer)
	targetOptions := terragruntOptions.Clone(targetConfig)
	targetOptions.TerraformCliArgs = []string{"output", "-json"}
	targetOptions.Writer = stdoutBufferWriter
	err := targetOptions.RunTerragrunt(targetOptions)
	if err != nil {
		return nil, errors.WithStackTrace(err)
	}

	stdoutBufferWriter.Flush()
	jsonString := stdoutBuffer.String()
	jsonBytes := []byte(jsonString)
	util.Debugf(terragruntOptions.Logger, "Retrieved output from %s as json: %s", targetConfig, jsonString)
	return jsonBytes, nil
}

// terraformOutputJsonToCtyValueMap takes the terraform output json and converts to a mapping between output keys to the
// parsed cty.Value encoding of the json objects.
func terraformOutputJsonToCtyValueMap(targetConfig string, jsonBytes []byte) (map[string]cty.Value, error) {
	// When getting all outputs, terraform returns a json with the data containing metadata about the types, so we
	// can't quite return the data directly. Instead, we will need further processing to get the output we want.
	// To do so, we first Unmarshal the json into a simple go map to a OutputMeta struct.
	type OutputMeta struct {
		Sensitive bool            `json:"sensitive"`
		Type      json.RawMessage `json:"type"`
		Value     json.RawMessage `json:"value"`
	}
	var outputs map[string]OutputMeta
	err := json.Unmarshal(jsonBytes, &outputs)
	if err != nil {
		return nil, errors.WithStackTrace(TerragruntOutputParsingError{Path: targetConfig, Err: err})
	}
	flattenedOutput := map[string]cty.Value{}
	for k, v := range outputs {
		outputType, err := ctyjson.UnmarshalType(v.Type)
		if err != nil {
			return nil, errors.WithStackTrace(TerragruntOutputParsingError{Path: targetConfig, Err: err})
		}
		outputVal, err := ctyjson.Unmarshal(v.Value, outputType)
		if err != nil {
			return nil, errors.WithStackTrace(TerragruntOutputParsingError{Path: targetConfig, Err: err})
		}
		flattenedOutput[k] = outputVal
	}
	return flattenedOutput, nil
}

// Custom error types

type DependencyConfigNotFound struct {
	Path string
}

func (err DependencyConfigNotFound) Error() string {
	return fmt.Sprintf("%s does not exist", err.Path)
}

type TerragruntOutputParsingError struct {
	Path string
	Err  error
}

func (err TerragruntOutputParsingError) Error() string {
	return fmt.Sprintf("Could not parse output from terragrunt config %s. Underlying error: %s", err.Path, err.Err)
}

type TerragruntOutputEncodingError struct {
	Path string
	Err  error
}

func (err TerragruntOutputEncodingError) Error() string {
	return fmt.Sprintf("Could not encode output from terragrunt config %s. Underlying error: %s", err.Path, err.Err)
}

type TerragruntOutputListEncodingError struct {
	Paths []string
	Err   error
}

func (err TerragruntOutputListEncodingError) Error() string {
	return fmt.Sprintf("Could not encode output from list of terragrunt configs %v. Underlying error: %s", err.Paths, err.Err)
}

type TerragruntOutputTargetNotApplied struct {
	targetConfig  string
	currentConfig string
}

func (err TerragruntOutputTargetNotApplied) Error() string {
	return fmt.Sprintf("%s is a dependency of %s but is not applied yet so can not get outputs.", err.targetConfig, err.currentConfig)
}
