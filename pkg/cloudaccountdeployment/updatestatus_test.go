package cloudaccountdeployment_test

import (
	"flag"
	"fmt"
	"github.com/optum/runiac/pkg/cloudaccountdeployment"
	"github.com/optum/runiac/pkg/config"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/afero"
)

var DefaultStubAccountID = "1"
var StubVersion = "v0.0.5"

var validate *validator.Validate
var fs afero.Fs
var logger *logrus.Entry
var stubConfig = config.Config{}

func TestMain(m *testing.M) {
	logger = logrus.NewEntry(logrus.New())
	fs = afero.NewMemMapFs()
	stubConfig.RegionGroup = "us"
	stubConfig.RegionalRegions = []string{"us-east-1", "us-east-2", "us-west-2"}
	stubConfig.Project = "project"
	stubConfig.UniqueExternalExecutionID = "taskID"
	stubConfig.AccountID = "accountID"

	flag.Parse()
	exitCode := m.Run()

	// Exit
	os.Exit(exitCode)
}

func TestFlushTracks_ShouldReturnCorrectSuccessesWithMultipleTracks(t *testing.T) {
	var mockedInput = map[int]interface{}{}

	stubTrackPrefix := "track"
	stubStepPrefix := "step"
	stubPrimaryRegion := "us-east-1"
	stubStepCount := 3

	for tI := 0; tI < 2; tI++ {
		stubTrack := fmt.Sprintf("%s%d", stubTrackPrefix, tI)
		for i := 0; i < stubStepCount; i++ {
			stubStep := fmt.Sprintf("%s-%d", stubStepPrefix, i)
			// primary start
			cloudaccountdeployment.RecordStepStart(logger, stubConfig.AccountID, stubTrack, stubStep, config.PrimaryRegionDeployType.String(), stubPrimaryRegion, stubConfig.DryRun, "", stubConfig.Version, stubConfig.UniqueExternalExecutionID, "", "", stubConfig.Project, stubConfig.RegionalRegions)

			// primary end
			cloudaccountdeployment.RecordStepSuccess(logger, "", stubTrack, stubStep, config.PrimaryRegionDeployType.String(), stubPrimaryRegion, stubConfig.UniqueExternalExecutionID, stubConfig.Project, stubConfig.RegionalRegions)

			// regional deploys
			for _, reg := range stubConfig.RegionalRegions {
				cloudaccountdeployment.RecordStepStart(logger, stubConfig.AccountID, stubTrack, stubStep, config.RegionalRegionDeployType.String(), reg, stubConfig.DryRun, "", stubConfig.Version, stubConfig.UniqueExternalExecutionID, "", "", stubConfig.Project, stubConfig.RegionalRegions)

				cloudaccountdeployment.RecordStepSuccess(logger, "", stubTrack, stubStep, config.RegionalRegionDeployType.String(), reg, stubConfig.UniqueExternalExecutionID, stubConfig.Project, stubConfig.RegionalRegions)
			}
		}
	}

	// reset mockedinput after setup
	mockedInput = map[int]interface{}{}

	flushedTrack := stubTrackPrefix + "0"
	steps, err := cloudaccountdeployment.FlushTrack(logger, flushedTrack)

	require.NoError(t, err)
	require.NotEmpty(t, steps)

	for _, v := range mockedInput {
		require.IsType(t, cloudaccountdeployment.UpdateRegionalStatusPayload{}, v)

		m, _ := v.(cloudaccountdeployment.UpdateRegionalStatusPayload)

		require.Equal(t, cloudaccountdeployment.Success.String(), m.Result)
		require.False(t, strings.HasPrefix(m.AccountStepDeploymentID, "#"), m.Result, "AccountStepDeploymentID does not contain a # prefix")
		require.Contains(t, m.AccountStepDeploymentID, flushedTrack, "AccountStepDeploymentID should contain steps from track being flushed: %s", flushedTrack)
		require.Empty(t, m.FailedRegions)
	}

	for _, v := range steps {
		require.Contains(t, v.AccountStepDeploymentID, flushedTrack, "AccountStepDeploymentID should contain steps from track being flushed: %s", flushedTrack)
	}

	noSteps, _ := cloudaccountdeployment.FlushTrack(logger, flushedTrack)
	require.Empty(t, noSteps, "FlushTrack should remove flushed steps")

	steps1, _ := cloudaccountdeployment.FlushTrack(logger, stubTrackPrefix+"1")
	require.NotEmpty(t, steps1, "FlushTrack should only remove steps to track being flushed")

}

func TestFlushTrack_ShouldReportAllStepsInSingleTrack(t *testing.T) {
	// arrange
	cloudaccountdeployment.StepDeployments = map[string]cloudaccountdeployment.ExecutionResult{
		"#logging#bridge_stream#primary#us-east-1": {
			Result:                  cloudaccountdeployment.Success,
			Region:                  "us-east-1",
			RegionDeployType:        "primary",
			AccountStepDeploymentID: "93d12293-3933-4d98-4b13-a8b357fb4697#CUSTOMER#logging#bridge_stream",
			CSP:                     "AZU",
			TargetRegions:           []string{"us-east-1"},
		},
		"#logging#flow_logs#primary#centralus": {
			Result:                  cloudaccountdeployment.Success,
			Region:                  "centralus",
			RegionDeployType:        "primary",
			AccountStepDeploymentID: "93d12293-3933-4d98-4b13-a8b357fb4697#CUSTOMER#logging#flow_logs",
			CSP:                     "AWS",
			TargetRegions:           []string{"us-east-1"},
		},
		"#logging#resource_groups#primary#centralus": {
			Result:                  cloudaccountdeployment.Success,
			Region:                  "centralus",
			RegionDeployType:        "primary",
			AccountStepDeploymentID: "93d12293-3933-4d98-4b13-a8b357fb4697#CUSTOMER#logging#resource_groups",
			CSP:                     "AZU",
			TargetRegions:           []string{"us-east-1"},
		},
	}

	var mockedInput = map[int]interface{}{}

	// act
	steps, err := cloudaccountdeployment.FlushTrack(logger, "logging")

	// assert
	require.NoError(t, err)
	require.NotEmpty(t, steps)

	// ensure result and accountstepdeploymentid are correct
	for _, v := range mockedInput {
		require.IsType(t, cloudaccountdeployment.UpdateRegionalStatusPayload{}, v)

		m, _ := v.(cloudaccountdeployment.UpdateRegionalStatusPayload)

		require.Equal(t, cloudaccountdeployment.Success.String(), m.Result)
		require.True(t, strings.HasPrefix(m.AccountStepDeploymentID, "93d12293-3933-4d98-4b13-a8b357fb4697#CUSTOMER#logging#"), m.Result, "AccountStepDeploymentID contains correct prefix")
	}
}
