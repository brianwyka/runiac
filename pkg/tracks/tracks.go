package tracks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/optum/runiac/pkg/cloudaccountdeployment"
	"github.com/optum/runiac/pkg/config"
	"github.com/optum/runiac/pkg/steps"
	"github.com/optum/runiac/plugins/terraform/pkg/terraform"
	"github.com/otiai10/copy"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
)

const (
	PRE_TRACK_NAME     = "_pretrack" // The name of the directory for the pretrack
	DEFAULT_TRACK_NAME = "default"   // The name of the default top-level track
)

// ExecuteTrackFunc facilitates track executions across multiple regions and RegionDeployTypes (e.g. Primary us-east-1 and regional us-*)
type ExecuteTrackFunc func(execution Execution, cfg config.Config, t Track, out chan<- Output)

// ExecuteTrackRegionFunc executes a track within a single region and RegionDeployType (e.g. primary/us-east-1 or regional/us-east-2)
type ExecuteTrackRegionFunc func(in <-chan RegionExecution, out chan<- RegionExecution)

type ExecuteStepFunc func(region string, regionDeployType config.RegionDeployType, entry *logrus.Entry, fs afero.Fs, defaultStepOutputVariables map[string]map[string]string, stepProgression int,
	s config.Step, out chan<- config.Step, destroy bool)

var DeployTrackRegion ExecuteTrackRegionFunc = ExecuteDeployTrackRegion
var DestroyTrackRegion ExecuteTrackRegionFunc = ExecuteDestroyTrackRegion

var DeployTrack ExecuteTrackFunc = ExecuteDeployTrack
var DestroyTrack ExecuteTrackFunc = ExecuteDestroyTrack

var ExecuteStep ExecuteStepFunc = ExecuteStepImpl

// Tracker is an interface for working with tracks
type Tracker interface {
	GatherTracks(config config.Config) (tracks []Track)
	ExecuteTracks(config config.Config) (output Stage)
}

// DirectoryBasedTracker implements the Tracker interface
type DirectoryBasedTracker struct {
	Log *logrus.Entry
	Fs  afero.Fs
}

// Track represents a delivery framework track (unit of functionality)
type Track struct {
	Name                        string
	Dir                         string
	StepProgressionsCount       int
	StepsCount                  int
	StepsWithTestsCount         int
	StepsWithRegionalTestsCount int
	RegionalDeployment          bool // If true at least one step is configured to deploy to multiple region
	OrderedSteps                map[int][]config.Step
	Output                      Output
	DestroyOutput               Output
	IsPreTrack                  bool // If true, this is a PreTrack, meaning it should be run before all other tracks
	IsDefaultTrack              bool // If true, this track represents steps contained in a standalone, top-level track
	Skipped                     bool // Indicates that the track was skipped. This will be for non-pretrack tracks if the pretrack fails
}

type Output struct {
	Name                       string
	PrimaryStepOutputVariables map[string]map[string]string
	Executions                 []RegionExecution
}

type Execution struct {
	Logger                              *logrus.Entry
	Fs                                  afero.Fs
	Output                              ExecutionOutput
	DefaultExecutionStepOutputVariables map[string]map[string]map[string]string
	PreTrackOutput                      *Output
}

type RegionExecution struct {
	TrackName                  string
	TrackDir                   string
	TrackStepProgressionsCount int
	TrackStepsWithTestsCount   int
	TrackOrderedSteps          map[int][]config.Step
	Logger                     *logrus.Entry
	Fs                         afero.Fs
	Output                     ExecutionOutput
	Region                     string
	RegionDeployType           config.RegionDeployType
	PrimaryOutput              ExecutionOutput // This value is only set when regiondeploytype == regional
	DefaultStepOutputVariables map[string]map[string]string
}

// TrackOutput represents the output from a track execution
type ExecutionOutput struct {
	Name                string
	Dir                 string
	ExecutedCount       int
	SkippedCount        int
	FailureCount        int
	FailedTestCount     int
	Steps               map[string]config.Step
	FailedSteps         []config.Step
	StepOutputVariables map[string]map[string]string // Output variables across all steps in the track. A map where K={step name} and V={map[outputVarName: outputVarVal]}
}

// Stage represents the outputs of tracks
type Stage struct {
	Tracks map[string]Track
}

// GatherTracks gets all tracks that should be executed based
// on the directory structure
func (tracker DirectoryBasedTracker) GatherTracks(config config.Config) (tracks []Track) {
	defaultDir := "./"
	tracksDir := "./tracks"
	defaultExists := false

	// try to read steps from the default track and step at the top-level directory, if it exists
	t, included, _ := tracker.readTrack(config, DEFAULT_TRACK_NAME, defaultDir)
	if included && t.StepsCount > 0 {
		defaultExists = true
		tracker.Log.Println(fmt.Sprintf("Tracks: Adding default track"))
		tracks = append(tracks, t)
	}

	// read tracks from the usual tracks directory
	items, _ := afero.ReadDir(tracker.Fs, tracksDir)
	for _, item := range items {
		if item.IsDir() {
			t, included, _ := tracker.readTrack(config, item.Name(), fmt.Sprintf("%s/%s", tracksDir, item.Name()))
			if included && t.StepsCount > 0 {
				tracker.Log.Println(fmt.Sprintf("Tracks: Adding %s", item.Name()))
				tracks = append(tracks, t)
			}
		}
	}

	// best practice is for one or the other of the above two situations to be present
	if defaultExists && len(tracks) > 1 {
		tracker.Log.Warnf("Detected that a default track (%s) exists along with one or more explicit tracks (%s). Best practice is to migrate your default track to a named one instead.", defaultDir, tracksDir)
	}

	return
}

func copyDefault(source, destination string) error {
	var err = filepath.Walk(source, func(path string, info os.FileInfo, err error) error {

		if strings.HasPrefix(path, "tracks") {
			return nil
		}

		var relPath = strings.Replace(path, source, "", 1)
		if relPath == "" {
			return nil
		}

		if strings.HasPrefix(relPath, "step") {
			fmt.Println(relPath)

			return copy.Copy(path, filepath.Join(destination, relPath))
		} else {
			return nil
		}
	})
	return err
}

func (tracker DirectoryBasedTracker) readTrack(cfg config.Config, name string, dir string) (Track, bool, error) {
	t := Track{
		Name:         name,
		Dir:          dir,
		OrderedSteps: map[int][]config.Step{},
	}

	if t.Name == PRE_TRACK_NAME {
		tracker.Log.Debug("Pre-track found")
		t.IsPreTrack = true
	} else if t.Name == DEFAULT_TRACK_NAME {
		tracker.Log.Debug("Default track found")
		t.IsDefaultTrack = true
	}

	if t.IsDefaultTrack {
		matches, _ := afero.Glob(tracker.Fs, "*.tf") // TODO(plugin): shift this check to a plugin to support more than terraform
		if len(matches) > 0 {
			_ = tracker.Fs.MkdirAll("./tracks/default/", 0755)
			err := copyDefault("./", "./tracks/default/")
			if err != nil {
				tracker.Log.WithError(err).Error("Failed to set up default track step")
				return t, false, err
			}
		}
	}

	// TODO(step:config)
	//tConfig := viper.New()
	//tConfig.SetConfigName("runiac")         // name of cfg file (without extension)
	//tConfig.AddConfigPath(filepath.Join(t.Dir)) // path to look for the cfg file in

	//if err := tConfig.ReadInConfig(); err != nil {
	//	if _, ok := err.(viper.ConfigFileNotFoundError); ok {
	//		// Config file not found, don't record or log error as this configuration file is optional.
	//		tracker.Log.Debug("Track is not using a runiac.yaml configuration file")
	//	} else {
	//		tracker.Log.WithError(err).Error("Error reading configuration file")
	//	}
	//}
	//
	//if tConfig.IsSet("enabled") && !tConfig.GetBool("enabled") {
	//	tracker.Log.Warningf("Skipping track %s. Not enabled in configuration.", t.Name)
	//	return t, false, nil
	//}

	// if steps are not being targeted and track are, skip the non-targeted tracks
	if len(cfg.StepWhitelist) == 0 && !cfg.TargetAll {
		tracker.Log.Warning(fmt.Sprintf("Tracks: Skipping %s", name))
		return t, false, nil
	} else {
		tFolders, _ := afero.ReadDir(tracker.Fs, t.Dir)
		stepPrefix := "step"
		highestProgressionLevel := 0

		for _, tFolder := range tFolders {
			tFolderName := tFolder.Name()

			// step folder convention is step{progressionLevel}_{stepName}
			if strings.HasPrefix(tFolderName, stepPrefix) {
				stepName := tFolderName[len(stepPrefix)+2:]

				// if the step belongs to the default track, exclude the name of the track from the identifier
				stepID := ""
				if t.IsDefaultTrack {
					stepID = fmt.Sprintf("#%s#%s", cfg.Project, stepName)
				} else {
					stepID = fmt.Sprintf("#%s#%s#%s", cfg.Project, t.Name, stepName)
				}

				// if step is not targeted, skip.
				if !contains(cfg.StepWhitelist, stepID) && !cfg.TargetAll {
					tracker.Log.Warningf("Step %s disabled. Not present in whitelist.", stepID)
					continue
				}

				parsedStringProgression := string(tFolderName[len(stepPrefix)])
				progressionLevel, err := strconv.Atoi(parsedStringProgression)

				if err != nil {
					tracker.Log.Error(err)
				}

				if progressionLevel > highestProgressionLevel {
					highestProgressionLevel = progressionLevel
				}

				step := config.Step{
					ProgressionLevel: progressionLevel,
					Name:             stepName,
					Dir:              filepath.Join(t.Dir, tFolderName),
					DeployConfig:     cfg,
					TrackName:        t.Name,
					ID:               stepID,
				}

				step.TestsExist = fileExists(tracker.Fs, filepath.Join(step.Dir, "tests/tests.test"))
				step.RegionalResourcesExist = exists(tracker.Fs, filepath.Join(step.Dir, "regional"))
				step.Runner = steps.DetermineRunner(step)

				if step.RegionalResourcesExist {
					step.RegionalTestsExist = fileExists(tracker.Fs, filepath.Join(step.Dir, "regional", "tests/tests.test"))
				}

				tracker.Log.Infof("Adding Step %s. Tests Exist: %v. Regional Resources Exist: %v. Regional Tests Exist: %v.", stepID, step.TestsExist, step.RegionalResourcesExist, step.RegionalTestsExist)

				// let track know it needs to execute regionally as well
				if !t.RegionalDeployment && step.RegionalResourcesExist {
					t.RegionalDeployment = true
				}

				t.OrderedSteps[progressionLevel] = append(t.OrderedSteps[progressionLevel], step)
				t.StepsCount++

				if step.TestsExist {
					t.StepsWithTestsCount++
				}

				if step.RegionalTestsExist {
					t.StepsWithRegionalTestsCount++
				}
			}
		}

		t.StepProgressionsCount = highestProgressionLevel
	}

	return t, true, nil
}

// fileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func fileExists(fs afero.Fs, filename string) bool {
	info, err := fs.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// isEmpty checks if a file or dir exists and is not empty
func exists(fs afero.Fs, filename string) bool {
	info, err := afero.IsEmpty(fs, filename)
	return err == nil && !info
}

// ExecuteTracks executes all tracks in parallel.
// If a _pretrack exists, this is executed before
// all other tracks.
func (tracker DirectoryBasedTracker) ExecuteTracks(cfg config.Config) (output Stage) {
	output.Tracks = map[string]Track{}
	var tracks = tracker.GatherTracks(cfg) // **All** tracks
	var parallelTracks []Track             // Tracks that should be executed in parallel

	// Pre track
	var preTrackExists bool
	var preTrack Track

	for _, t := range tracks {
		output.Tracks[t.Name] = t
		if t.IsPreTrack {
			preTrackExists = true
			preTrack = t
		} else {
			parallelTracks = append(parallelTracks, t)
		}
	}

	// Execute _pretrack if it exists
	if preTrackExists {
		tracker.Log.Debug("Pre-track execution starting")

		preTrackChan := make(chan Output)
		preTrackExecution := Execution{
			Logger:                              tracker.Log,
			Fs:                                  tracker.Fs,
			Output:                              ExecutionOutput{},
			DefaultExecutionStepOutputVariables: map[string]map[string]map[string]string{},
		}
		go DeployTrack(preTrackExecution, cfg, preTrack, preTrackChan)
		// Wait for the track to contain an item,
		// indicating the track has completed.
		preTrackOutput := <-preTrackChan
		preTrack.Output = preTrackOutput
		output.Tracks[preTrack.Name] = preTrack
		tracker.Log.Debug("Pre-track finished")
		// If any of the pretrack's executions has a step failure,
		// the pretrack is considered failed
		// so we cannot continue with the other tracks
		for _, exec := range preTrackOutput.Executions {
			for _, step := range exec.Output.Steps {
				if step.Output.Status == config.Fail {
					tracker.Log.Error("Pre-track failed, subsequent tracks will not be executed")
					// Mark all other tracks as skipped
					for _, track := range output.Tracks {
						if track.Name != PRE_TRACK_NAME {
							track.Skipped = true
							output.Tracks[track.Name] = track
						}
					}
					return
				}
			}
		}
	}

	// Execute non pre/post tracks in parallel
	numParallelTracks := len(parallelTracks)
	parallelTrackChan := make(chan Output)

	// execute all tracks concurrently
	// within ExecuteDeployTrack, track result will be added to trackChan feeding next loop
	for _, t := range parallelTracks {
		execution := Execution{
			Logger:                              tracker.Log,
			Fs:                                  tracker.Fs,
			Output:                              ExecutionOutput{},
			DefaultExecutionStepOutputVariables: map[string]map[string]map[string]string{},
		}
		// If there is a pretrack, add its outputs
		// to the execution so they are available.
		if preTrackExists {
			execution.PreTrackOutput = &preTrack.Output
		}
		go DeployTrack(execution, cfg, t, parallelTrackChan)
	}

	// wait for all executions to finish (this loop matches above range)
	for tExecution := 0; tExecution < numParallelTracks; tExecution++ {
		// waiting to append <-trackChan Track N times will inherently wait for all above executions to finish
		tOutput := <-parallelTrackChan
		if t, ok := output.Tracks[tOutput.Name]; ok {
			// TODO: is it better to have a pointer for map value?
			t.Output = tOutput
			output.Tracks[tOutput.Name] = t
		}
	}

	// If SelfDestroy or Destroy is set (e.g. during PRs), destroy any resources created by the tracks
	if cfg.SelfDestroy && !cfg.DryRun {
		tracker.Log.Info("Executing destroy...")
		trackDestroyChan := make(chan Output)

		for _, t := range parallelTracks {
			executionStepOutputVariables := map[string]map[string]map[string]string{}

			for _, exec := range output.Tracks[t.Name].Output.Executions {
				executionStepOutputVariables[fmt.Sprintf("%s-%s", exec.RegionDeployType, exec.Region)] = exec.Output.StepOutputVariables
			}

			if tracker.Log.Level == logrus.DebugLevel {
				jsonBytes, _ := json.Marshal(executionStepOutputVariables)

				tracker.Log.Debugf("OUTPUT VARS: %s", string(jsonBytes))
			}

			execution := Execution{
				Logger:                              tracker.Log,
				Fs:                                  tracker.Fs,
				Output:                              ExecutionOutput{},
				DefaultExecutionStepOutputVariables: executionStepOutputVariables,
			}
			// If there is a pretrack, add its outputs
			// to the execution so they are available.
			if preTrackExists {
				execution.PreTrackOutput = &preTrack.Output
			}
			go DestroyTrack(execution, cfg, t, trackDestroyChan)
		}

		// wait for all executions to finish (this loop matches above range)
		for range parallelTracks {
			// waiting to append <-trackDestroyChan Track N times will inherently wait for all above executions to finish
			tDestroyOutout := <-trackDestroyChan

			if t, ok := output.Tracks[tDestroyOutout.Name]; ok {
				// TODO: is it better to have a pointer for map value?
				t.DestroyOutput = tDestroyOutout
				output.Tracks[tDestroyOutout.Name] = t
			}
		}

		// Destroy _pretrack if it exists
		if preTrackExists {
			tracker.Log.Debug("Pre-track destroying")
			executionStepOutputVariables := map[string]map[string]map[string]string{}

			for _, exec := range output.Tracks[preTrack.Name].Output.Executions {
				executionStepOutputVariables[fmt.Sprintf("%s-%s", exec.RegionDeployType, exec.Region)] = exec.Output.StepOutputVariables
			}

			destroyPreTrackChan := make(chan Output)
			preTrackDestroyExecution := Execution{
				Logger:                              tracker.Log,
				Fs:                                  tracker.Fs,
				Output:                              ExecutionOutput{},
				DefaultExecutionStepOutputVariables: executionStepOutputVariables,
				PreTrackOutput:                      &preTrack.Output,
			}
			go DestroyTrack(preTrackDestroyExecution, cfg, preTrack, destroyPreTrackChan)
			// Wait for the track to contain an item,
			// indicating the track has been destroyed.
			preTrackDestroyOutput := <-destroyPreTrackChan
			preTrack.DestroyOutput = preTrackDestroyOutput
			tracker.Log.Debug("Pre-track destroy finished")
			if t, ok := output.Tracks[preTrackDestroyOutput.Name]; ok {
				t.DestroyOutput = preTrackDestroyOutput
				output.Tracks[preTrackDestroyOutput.Name] = t
			}
		}
	}

	return
}

// Adds step outputs variables to the track output variables map
// K = Step Name, V = map[StepOutputVarName: StepOutputVarValue]
func AppendTrackOutput(trackOutputVariables map[string]map[string]string, output config.StepOutput) map[string]map[string]string {

	key := output.StepName

	if output.RegionDeployType == config.RegionalRegionDeployType {
		key = fmt.Sprintf("%s-%s", key, output.RegionDeployType.String())
	}

	if trackOutputVariables[key] == nil {
		trackOutputVariables[key] = make(map[string]string)
	}

	for k, v := range output.OutputVariables {
		trackOutputVariables[key][k] = terraform.OutputToString(v)
	}

	return trackOutputVariables
}

func AppendPreTrackOutputsToDefaultStepOutputVariables(defaultStepOutputVariables map[string]map[string]string, preTrackOutput *Output, regionDeployType config.RegionDeployType, region string) map[string]map[string]string {
	for _, execution := range preTrackOutput.Executions {
		if execution.RegionDeployType == regionDeployType && execution.Region == region {
			for step, outputVarMap := range execution.Output.StepOutputVariables {
				for outVarName, outVarVal := range outputVarMap {
					key := fmt.Sprintf("pretrack-%s", step)

					// Check if the key already exists
					if _, ok := defaultStepOutputVariables[key]; ok {
						defaultStepOutputVariables[key][outVarName] = outVarVal
					} else {
						defaultStepOutputVariables[key] = map[string]string{
							outVarName: outVarVal,
						}
					}
				}
			}
		}
	}

	return defaultStepOutputVariables
}

// ExecuteDeployTrack is for executing a single track across regions
func ExecuteDeployTrack(execution Execution, cfg config.Config, t Track, out chan<- Output) {
	logger := execution.Logger.WithFields(logrus.Fields{
		"track":  t.Name,
		"action": "deploy",
	})

	output := Output{
		Name:                       t.Name,
		Executions:                 []RegionExecution{},
		PrimaryStepOutputVariables: map[string]map[string]string{},
	}

	primaryOutChan := make(chan RegionExecution, 1)
	primaryInChan := make(chan RegionExecution, 1)

	region := cfg.PrimaryRegion // TODO(cfg:region): allow this to be overridden

	primaryRegionExecution := RegionExecution{
		TrackName:                  t.Name,
		TrackDir:                   t.Dir,
		TrackStepProgressionsCount: t.StepProgressionsCount,
		TrackStepsWithTestsCount:   t.StepsWithTestsCount,
		TrackOrderedSteps:          t.OrderedSteps,
		Logger:                     logger,
		Fs:                         execution.Fs,
		Output:                     ExecutionOutput{},
		Region:                     region,
		RegionDeployType:           config.PrimaryRegionDeployType,
		DefaultStepOutputVariables: map[string]map[string]string{},
	}

	if val, ok := execution.DefaultExecutionStepOutputVariables[fmt.Sprintf("%s-%s", primaryRegionExecution.RegionDeployType, primaryRegionExecution.Region)]; ok {
		primaryRegionExecution.DefaultStepOutputVariables = val
	}

	// Add step outputs for primary steps
	// from the pretrack
	if execution.PreTrackOutput != nil {
		primaryRegionExecution.DefaultStepOutputVariables = AppendPreTrackOutputsToDefaultStepOutputVariables(primaryRegionExecution.DefaultStepOutputVariables, execution.PreTrackOutput, primaryRegionExecution.RegionDeployType, primaryRegionExecution.Region)
	}

	go DeployTrackRegion(primaryInChan, primaryOutChan)
	primaryInChan <- primaryRegionExecution

	primaryTrackExecution := <-primaryOutChan
	output.Executions = append(output.Executions, primaryTrackExecution)
	output.PrimaryStepOutputVariables = primaryTrackExecution.Output.StepOutputVariables

	// end early if track has no regional step resources
	if !t.RegionalDeployment {
		logger.Info("Track has no regional resources, completing track.")
		_, err := cloudaccountdeployment.FlushTrack(logger, t.Name)

		if err != nil {
			logger.WithError(err).Error(err)
		}

		out <- output
		return
	}

	targetRegions := cfg.RegionalRegions // TODO(cfg:region): allow this to be overridden
	targetRegionsCount := len(targetRegions)
	regionOutChan := make(chan RegionExecution, targetRegionsCount)
	regionInChan := make(chan RegionExecution, targetRegionsCount)

	logger.Infof("Primary region successfully completed, executing regional deployments in %v.", targetRegions)

	for i := 0; i < targetRegionsCount; i++ {
		go DeployTrackRegion(regionInChan, regionOutChan)
	}

	for _, reg := range targetRegions {
		outputVars := map[string]map[string]string{}

		// Like slices, maps hold references to an underlying data structure. If you pass a map to a function that changes the contents of the map, the changes will be visible in the caller.
		// https://golang.org/doc/effective_go.html#maps
		// While map is being used for StepOutputVariables, required to copyDefault value to a new map to avoid regions overwriting each other while inflight regional step variables are added
		for k, v := range primaryTrackExecution.Output.StepOutputVariables {
			outputVars[k] = v
		}

		regionalRegionExecution := RegionExecution{
			TrackName:                  t.Name,
			TrackDir:                   t.Dir,
			TrackStepProgressionsCount: t.StepProgressionsCount,
			TrackStepsWithTestsCount:   t.StepsWithRegionalTestsCount,
			TrackOrderedSteps:          t.OrderedSteps,
			Logger:                     logger,
			Fs:                         execution.Fs,
			Output:                     ExecutionOutput{},
			Region:                     reg,
			RegionDeployType:           config.RegionalRegionDeployType,
			DefaultStepOutputVariables: outputVars,
			PrimaryOutput:              primaryTrackExecution.Output,
		}

		// Add step outputs for regional steps
		// from the pretrack
		if execution.PreTrackOutput != nil {
			regionalRegionExecution.DefaultStepOutputVariables = AppendPreTrackOutputsToDefaultStepOutputVariables(regionalRegionExecution.DefaultStepOutputVariables, execution.PreTrackOutput, regionalRegionExecution.RegionDeployType, regionalRegionExecution.Region)
		}

		regionInChan <- regionalRegionExecution
	}

	for i := 0; i < targetRegionsCount; i++ {
		regionTrackOutput := <-regionOutChan
		output.Executions = append(output.Executions, regionTrackOutput)
	}

	stepExecutions, err := cloudaccountdeployment.FlushTrack(logger, t.Name)

	if err != nil {
		logger.WithError(err).Error(err)
	}

	if logger.Level == logrus.DebugLevel {
		json, _ := json.Marshal(stepExecutions)

		logger.Debug(string(json))
	}

	out <- output
}

// ExecuteDestroyTrack is a helper function for destroying a track
func ExecuteDestroyTrack(execution Execution, cfg config.Config, t Track, out chan<- Output) {
	trackLogger := execution.Logger.WithFields(logrus.Fields{
		"track":  t.Name,
		"action": "destroy",
	})

	output := Output{
		Name:       t.Name,
		Executions: []RegionExecution{},
	}

	// TODO(high): need to gather previous step variables before attempting to destroy!

	// start with regional if existing
	if t.RegionalDeployment {
		regionOutChan := make(chan RegionExecution)
		regionInChan := make(chan RegionExecution)

		targetRegions := cfg.RegionalRegions
		targetRegionsCount := len(cfg.RegionalRegions)

		for i := 0; i < targetRegionsCount; i++ {
			go DestroyTrackRegion(regionInChan, regionOutChan)
		}

		for _, reg := range targetRegions {
			regionExecution := RegionExecution{
				TrackName:                  t.Name,
				TrackDir:                   t.Dir,
				TrackStepProgressionsCount: t.StepProgressionsCount,
				TrackOrderedSteps:          t.OrderedSteps,
				Logger:                     trackLogger,
				Fs:                         execution.Fs,
				Output:                     ExecutionOutput{},
				Region:                     reg,
				RegionDeployType:           config.RegionalRegionDeployType,
				DefaultStepOutputVariables: execution.DefaultExecutionStepOutputVariables[fmt.Sprintf("%s-%s", config.RegionalRegionDeployType, reg)],
			}

			// Add step outputs for regional steps
			// from the pretrack
			if execution.PreTrackOutput != nil {
				regionExecution.DefaultStepOutputVariables = AppendPreTrackOutputsToDefaultStepOutputVariables(regionExecution.DefaultStepOutputVariables, execution.PreTrackOutput, regionExecution.RegionDeployType, regionExecution.Region)
			}

			regionInChan <- regionExecution
		}

		for i := 0; i < targetRegionsCount; i++ {
			regionTrackOutput := <-regionOutChan
			output.Executions = append(output.Executions, regionTrackOutput)
		}
	}

	// clean up primary
	primaryOutChan := make(chan RegionExecution, 1)
	primaryInChan := make(chan RegionExecution, 1)

	region := cfg.PrimaryRegion // TODO(cfg:region): allow this to be overridden

	primaryExecution := RegionExecution{
		TrackName:                  t.Name,
		TrackDir:                   t.Dir,
		TrackStepProgressionsCount: t.StepProgressionsCount,
		TrackOrderedSteps:          t.OrderedSteps,
		Logger:                     trackLogger,
		Fs:                         execution.Fs,
		Output:                     ExecutionOutput{},
		Region:                     region,
		RegionDeployType:           config.PrimaryRegionDeployType,
		DefaultStepOutputVariables: execution.DefaultExecutionStepOutputVariables[fmt.Sprintf("%s-%s", config.PrimaryRegionDeployType, region)],
	}

	// Add step outputs for primary steps
	// from the pretrack
	if execution.PreTrackOutput != nil {
		primaryExecution.DefaultStepOutputVariables = AppendPreTrackOutputsToDefaultStepOutputVariables(primaryExecution.DefaultStepOutputVariables, execution.PreTrackOutput, primaryExecution.RegionDeployType, primaryExecution.Region)
	}

	go DestroyTrackRegion(primaryInChan, primaryOutChan)
	primaryInChan <- primaryExecution

	primaryTrackOutput := <-primaryOutChan
	output.Executions = append(output.Executions, primaryTrackOutput)

	out <- output
}

func ExecuteDeployTrackRegion(in <-chan RegionExecution, out chan<- RegionExecution) {
	execution := <-in
	logger := execution.Logger.WithFields(logrus.Fields{
		"region":           execution.Region,
		"regionDeployType": execution.RegionDeployType.String(),
	})

	execution.Output = ExecutionOutput{
		Name:                execution.TrackName,
		Dir:                 execution.TrackDir,
		Steps:               map[string]config.Step{},
		StepOutputVariables: execution.DefaultStepOutputVariables,
	}

	if execution.Output.StepOutputVariables == nil {
		execution.Output.StepOutputVariables = map[string]map[string]string{}
	}

	// define test channel outside of stepProgression loop to allow tests to run in background while steps proceed through progressions
	testOutChan := make(chan config.StepTestOutput)
	testInChan := make(chan config.Step)

	// Create testing goroutines.
	for testExecution := 0; testExecution < execution.TrackStepsWithTestsCount; testExecution++ {
		go executeStepTest(logger, execution.Fs, execution.Region, execution.RegionDeployType, execution.Output.StepOutputVariables, testInChan, testOutChan)
	}

	for progressionLevel := 1; progressionLevel <= execution.TrackStepProgressionsCount; progressionLevel++ {
		sChan := make(chan config.Step)
		for _, s := range execution.TrackOrderedSteps[progressionLevel] {

			// regional resources do not exist
			if execution.RegionDeployType == config.RegionalRegionDeployType && !s.RegionalResourcesExist {
				go func(s config.Step) {
					s.Output.Status = config.Na
					sChan <- s
				}(s)
				// if any previous failures, skip
			} else if progressionLevel > 1 && execution.Output.FailureCount > 0 {
				go func(s config.Step, logger *logrus.Entry) {
					slogger := logger.WithFields(logrus.Fields{
						"step": s.Name,
					})

					slogger.Warn("Skipping step due to earlier step failures in this region")

					s.Output.Status = config.Skipped
					sChan <- s
				}(s, logger)
			} else if execution.PrimaryOutput.FailureCount > 0 {
				go func(s config.Step, logger *logrus.Entry) {
					slogger := logger.WithFields(logrus.Fields{
						"step": s.Name,
					})

					slogger.Warn("Skipping step due to failures in primary region deployment")

					s.Output.Status = config.Skipped
					sChan <- s
				}(s, logger)
			} else {
				go ExecuteStep(execution.Region, execution.RegionDeployType, logger, execution.Fs, execution.Output.StepOutputVariables, progressionLevel, s, sChan, false)
			}
		}

		N := len(execution.TrackOrderedSteps[progressionLevel])
		for i := 0; i < N; i++ {
			s := <-sChan
			if s.Output.Status == config.Skipped {
				execution.Output.SkippedCount++
			} else {
				execution.Output.ExecutedCount++
			}
			execution.Output.Steps[s.Name] = s
			execution.Output.StepOutputVariables = AppendTrackOutput(execution.Output.StepOutputVariables, s.Output)

			if s.Output.Err != nil || s.Output.Status == config.Fail {
				execution.Output.FailureCount++
				execution.Output.FailedSteps = append(execution.Output.FailedSteps, s)
			}

			// trigger tests if exist, this number needs to match testing goroutines triggered above
			// further filtering happens after trigger
			if execution.RegionDeployType == config.RegionalRegionDeployType && s.RegionalTestsExist {
				logger.Debug("Triggering tests")
				testInChan <- s
			} else if execution.RegionDeployType == config.PrimaryRegionDeployType && s.TestsExist {
				logger.Debug("Triggering tests")
				testInChan <- s
			}
		}
	}

	for testExecution := 0; testExecution < execution.TrackStepsWithTestsCount; testExecution++ {
		s := <-testOutChan

		// add test output to trackOut
		if val, ok := execution.Output.Steps[s.StepName]; ok {
			// TODO: is it better to have a pointer for map value?
			val.TestOutput = s
			execution.Output.Steps[s.StepName] = val
		}

		// TODO: avoid this loop with FailedSteps
		for i := range execution.Output.FailedSteps {
			if execution.Output.FailedSteps[i].Name == s.StepName {
				execution.Output.FailedSteps[i].TestOutput = s
			}
		}

		if s.Err != nil {
			execution.Output.FailedTestCount++
		}
	}

	out <- execution
}

func ExecuteDestroyTrackRegion(in <-chan RegionExecution, out chan<- RegionExecution) {
	execution := <-in

	logger := execution.Logger.WithFields(logrus.Fields{
		"region":           execution.Region,
		"regionDeployType": execution.RegionDeployType.String(),
	})

	execution.Output = ExecutionOutput{
		Name:                execution.TrackName,
		Dir:                 execution.TrackDir,
		Steps:               map[string]config.Step{},
		StepOutputVariables: execution.DefaultStepOutputVariables,
	}

	for i := execution.TrackStepProgressionsCount; i >= 1; i-- {
		sChan := make(chan config.Step)
		for progressionLevel, s := range execution.TrackOrderedSteps[i] {
			// if any previous failures, skip
			if (progressionLevel > 1 && execution.Output.FailureCount > 0) || (execution.RegionDeployType == config.RegionalRegionDeployType && !s.RegionalResourcesExist) {
				go func(s config.Step) {
					s.Output.Status = config.Skipped
					sChan <- s
				}(s)
			} else {
				go ExecuteStep(execution.Region, execution.RegionDeployType, logger, execution.Fs, execution.Output.StepOutputVariables, i, s, sChan, true)
			}
		}
		N := len(execution.TrackOrderedSteps[i])
		for i := 0; i < N; i++ {
			s := <-sChan
			if s.Output.Status == config.Skipped {
				execution.Output.SkippedCount++
			} else {
				execution.Output.ExecutedCount++
			}
			execution.Output.Steps[s.Name] = s

			if s.Output.Err != nil {
				execution.Output.FailureCount++
				execution.Output.FailedSteps = append(execution.Output.FailedSteps, s)
			}
		}
	}

	out <- execution
	return
}

func ExecuteStepImpl(region string, regionDeployType config.RegionDeployType,
	logger *logrus.Entry, fs afero.Fs, defaultStepOutputVariables map[string]map[string]string, stepProgression int,
	s config.Step, out chan<- config.Step, destroy bool) {

	exec, err := steps.InitExecution(s, logger, fs, regionDeployType, region, defaultStepOutputVariables)

	// if error initializing, short circuit
	if err != nil {
		s.Output = config.StepOutput{
			Status:           config.Fail,
			RegionDeployType: regionDeployType,
			Region:           region,
			StepName:         s.Name,
			StreamOutput:     "",
			Err:              err,
			OutputVariables:  nil,
		}
		out <- s
		return
	}

	var output config.StepOutput

	exec2, _ := s.Runner.PreExecute(exec)

	if destroy {
		output = steps.ExecuteStepDestroy(s.Runner, exec2)
	} else {
		output = steps.ExecuteStep(s.Runner, exec2)
	}

	s.Output = output

	out <- s
	return
}

func executeStepTest(incomingLogger *logrus.Entry, fs afero.Fs, region string, regionDeployType config.RegionDeployType, defaultStepOutputVariables map[string]map[string]string, in <-chan config.Step, out chan<- config.StepTestOutput) {
	s := <-in
	tOutput := config.StepTestOutput{}

	logger := incomingLogger.WithFields(logrus.Fields{
		"step":            s.Name,
		"stepProgression": s.ProgressionLevel,
		"action":          "test",
	})

	logger.Info("Starting Step Tests")

	// only run step tests when they exist and deployment was error free
	if s.Output.Err != nil || s.Output.Status == config.Fail {
		logger.Warn("Skipping Tests Due to Deployment Error")
	} else if s.DeployConfig.DryRun {
		logger.Info("Skipping Tests for Dry Run")
	} else if s.Output.Status == config.Skipped {
		logger.Warn("Skipping Tests because step was also skipped")
	} else {
		logger.Info("Triggering Step Tests")
		exec, err := steps.InitExecution(s, logger, fs, regionDeployType, region, defaultStepOutputVariables)

		// if err initializing, short circuit
		if err != nil {
			tOutput = config.StepTestOutput{
				StepName:     s.Name,
				StreamOutput: "",
				Err:          err,
			}

			out <- tOutput
			return
		}

		tOutput = s.Runner.ExecuteStepTests(exec)

		if tOutput.Err != nil {
			logger.WithError(tOutput.Err).Error("Error executing tests for step")
		}
	}

	out <- tOutput
	return
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if strings.ToLower(a) == strings.ToLower(e) {
			return true
		}
	}
	return false
}
