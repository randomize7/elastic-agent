// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//nolint:dupl // duplicate code is in test cases
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	gproto "google.golang.org/protobuf/proto"

	fakecmp "github.com/elastic/elastic-agent/pkg/component/fake/component/comp"

	"github.com/gofrs/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.elastic.co/apm/apmtest"

	"github.com/elastic/elastic-agent-client/v7/pkg/client"
	"github.com/elastic/elastic-agent-client/v7/pkg/proto"
	"github.com/elastic/elastic-agent-libs/logp"

	"github.com/elastic/elastic-agent/internal/pkg/agent/application/info"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/paths"
	"github.com/elastic/elastic-agent/internal/pkg/agent/configuration"
	"github.com/elastic/elastic-agent/pkg/component"
	"github.com/elastic/elastic-agent/pkg/core/logger"
	"github.com/elastic/elastic-agent/pkg/features"
)

const (
	exeExt             = ".exe"
	errActionUndefined = "action undefined"
)

var (
	fakeInputSpec = component.InputSpec{
		Name: "fake",
		Command: &component.CommandSpec{
			Timeouts: component.CommandTimeoutSpec{
				Checkin: 30 * time.Second,
				Restart: 10 * time.Millisecond, // quick restart during tests
				Stop:    30 * time.Second,
			},
		},
	}
	fakeShipperSpec = component.ShipperSpec{
		Name: "fake-shipper",
		Command: &component.CommandSpec{
			Timeouts: component.CommandTimeoutSpec{
				Checkin: 30 * time.Second,
				Restart: 10 * time.Millisecond, // quick restart during tests
				Stop:    30 * time.Second,
			},
		},
	}
)

func TestManager_SimpleComponentErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		ai,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig(),
	)
	require.NoError(t, err)

	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	comp := component.Component{
		ID:  "error-default",
		Err: errors.New("hard-coded error"),
		Units: []component.Unit{
			{
				ID:     "error-input",
				Type:   client.UnitTypeInput,
				Config: nil,
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		sub := m.Subscribe(subCtx, "error-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateStarting {
					// initial is starting
				} else if state.State == client.UnitStateFailed {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "error-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							// should be failed
							subErrCh <- nil
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					}
				} else {
					subErrCh <- fmt.Errorf("component reported unexpected state: %v", state.State)
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_StartStop(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							// remove the component which will stop it
							m.Update(component.Model{Components: []component.Component{}})
							err := <-m.errCh
							if err != nil {
								subErrCh <- err
							}
						} else if unit.State == client.UnitStateStopped {
							subErrCh <- nil
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)

	workDir := filepath.Join(paths.Run(), comp.ID)
	_, err = os.Stat(workDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestManager_FakeInput_Features(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentInfo, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		agentInfo,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig())
	require.NoError(t, err)

	managerErrCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		managerErrCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	const compID = "fake-default"
	comp := component.Component{
		ID: compID,
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subscriptionCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subscriptionErrCh := make(chan error)
	doneCh := make(chan struct{})

	go func() {
		sub := m.Subscribe(subscriptionCtx, compID)
		var healthIteration int

		for {
			select {
			case <-subscriptionCtx.Done():
				return
			case componentState := <-sub.Ch():
				t.Logf("component state changed: %+v", componentState)

				if componentState.State == client.UnitStateFailed {
					subscriptionErrCh <- fmt.Errorf("component failed: %s", componentState.Message)
					return
				}

				unit, ok := componentState.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
				if !ok {
					subscriptionErrCh <- errors.New("unit missing: fake-input")
					return
				}

				switch unit.State {
				case client.UnitStateFailed:
					subscriptionErrCh <- fmt.Errorf("unit failed: %s", unit.Message)

				case client.UnitStateHealthy:
					healthIteration++
					switch healthIteration {
					case 1: // yes, it's starting on 1
						comp.Features = &proto.Features{
							Fqdn: &proto.FQDNFeature{Enabled: true},
						}

						m.Update(component.Model{Components: []component.Component{comp}})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthIteration, err)
							return
						}

					// check if config sent on iteration 1 was set
					case 2:
						// In the previous iteration, the (fake) component has received a CheckinExpected
						// message to enable the feature flag for FQDN.  In this iteration we are about to
						// retrieve the feature flags information from the same component via the retrieve_features
						// action. Within the component, which is running as a separate process, actions
						// and CheckinExpected messages are processed concurrently.  We need some way to wait
						// a reasonably short amount of time for the CheckinExpected message to be applied by the
						// component (thus setting the FQDN feature flag to true) before we as the same component
						// for feature flags information.  We accomplish this via assert.Eventually.
						assert.Eventuallyf(t, func() bool {
							// check the component
							res, err := m.PerformAction(
								context.Background(),
								comp,
								comp.Units[0],
								fakecmp.ActionRetrieveFeatures,
								nil)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to PerformAction %s: %w",
									healthIteration, fakecmp.ActionRetrieveFeatures, err)
								return false
							}

							ff, err := features.Parse(map[string]any{"agent": res})
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to parse action %s response as features config: %w",
									healthIteration, fakecmp.ActionRetrieveFeatures, err)
								return false
							}

							return ff.FQDN()
						}, 1*time.Second, 100*time.Millisecond, "failed to assert that FQDN feature flag was enabled by component")

						doneCh <- struct{}{}
					}

				case client.UnitStateStarting:
					// acceptable

				case client.UnitStateConfiguring:
					// set unit back to healthy, so other cases will run.
					comp.Units[0].Config = component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy",
					})

					m.Update(component.Model{Components: []component.Component{comp}})
					err := <-m.errCh
					if err != nil {
						t.Logf("error updating component state to health: %v", err)

						subscriptionErrCh <- fmt.Errorf("failed to update component: %w", err)
					}

				default:
					// unexpected state that should not have occurred
					subscriptionErrCh <- fmt.Errorf("unit reported unexpected state: %v",
						unit.State)
				}

			}
		}
	}()

	defer drainErrChan(managerErrCh)
	defer drainErrChan(subscriptionErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	timeout := 30 * time.Second
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	// Wait for a success, an error or time out
	for {
		select {
		case <-timeoutTimer.C:
			t.Fatalf("timed out after %s", timeout)
		case err := <-managerErrCh:
			require.NoError(t, err)
		case err := <-subscriptionErrCh:
			require.NoError(t, err)
		case <-doneCh:
			subCancel()
			cancel()

			err = <-managerErrCh
			require.NoError(t, err)
			return
		}
	}
}

func TestManager_FakeInput_APM(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentInfo, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		agentInfo,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig())
	require.NoError(t, err)

	managerErrCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		managerErrCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	const compID = "fake-default"
	comp := component.Component{
		ID: compID,
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subscriptionCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subscriptionErrCh := make(chan error)
	doneCh := make(chan struct{})

	initialAPMConfig := &proto.APMConfig{
		Elastic: &proto.ElasticAPM{
			Environment: "test",
			ApiKey:      "apiKey",
			SecretToken: "secretToken",
			Hosts:       []string{"host1", "host2", "host3"},
			Tls: &proto.ElasticAPMTLS{
				SkipVerify: true,
				ServerCert: "servercert",
				ServerCa:   "serverca",
			},
		},
	}

	modifiedAPMConfig := &proto.APMConfig{
		Elastic: &proto.ElasticAPM{
			Environment: "test-modified",
			ApiKey:      "apiKey",
			SecretToken: "secretToken",
			Hosts:       []string{"newhost1", "host2", "differenthost3"},
			Tls: &proto.ElasticAPMTLS{
				SkipVerify: true,
				ServerCert: "",
				ServerCa:   "",
			},
		},
	}

	go func() {
		sub := m.Subscribe(subscriptionCtx, compID)
		var healthIteration int
		var retrievedApmConfig *proto.APMConfig
		for {
			select {
			case <-subscriptionCtx.Done():
				return
			case componentState := <-sub.Ch():
				t.Logf("component state changed: %+v", componentState)

				if componentState.State == client.UnitStateFailed {
					subscriptionErrCh <- fmt.Errorf("component failed: %s", componentState.Message)
					return
				}

				unit, ok := componentState.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
				if !ok {
					subscriptionErrCh <- errors.New("unit missing: fake-input")
					return
				}

				switch unit.State {
				case client.UnitStateFailed:
					subscriptionErrCh <- fmt.Errorf("unit failed: %s", unit.Message)

				case client.UnitStateHealthy:
					healthIteration++
					t.Logf("Healthy iteration %d starting at %s", healthIteration, time.Now())
					switch healthIteration {
					case 1: // yes, it's starting on 1
						comp.Component = &proto.Component{
							ApmConfig: initialAPMConfig,
						}
						m.Update(component.Model{Components: []component.Component{comp}})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthIteration, err)
							return
						}

					// check if config sent on iteration 1 was set
					case 2:
						// In the previous iteration, the (fake) component has received a CheckinExpected
						// message to propagate the APM configuration. In this iteration we are about to
						// retrieve the APM configuration from the same component via the retrieve_apm_config
						// action. Within the component, which is running as a separate process, actions
						// and CheckinExpected messages are processed concurrently. We need some way to wait
						// a reasonably short amount of time for the CheckinExpected message to be applied by the
						// component (thus setting the APM config) before we query the same component
						// for apm config information. We accomplish this via assert.Eventually.
						// We also send a modified APM config to see that the component updates correctly and
						// reports the new config in the next iteration.
						assert.Eventuallyf(t, func() bool {
							// check the component
							res, err := m.PerformAction(
								context.Background(),
								comp,
								comp.Units[0],
								fakecmp.ActionRetrieveAPMConfig,
								nil)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to PerformAction %s: %w",
									healthIteration, fakecmp.ActionRetrieveAPMConfig, err)
								return false
							}
							retrievedApmConfig, err = extractAPMConfigFromActionResult(t, res)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to retrieve APM Config from ActionResult %s: %w",
									healthIteration, fakecmp.ActionRetrieveAPMConfig, err)
								return false
							}
							return gproto.Equal(initialAPMConfig, retrievedApmConfig)
						}, 1*time.Second, 100*time.Millisecond, "APM config was not received by component. expected: %s actual: %s", initialAPMConfig, retrievedApmConfig)

						comp.Component = &proto.Component{
							ApmConfig: modifiedAPMConfig,
						}
						m.Update(component.Model{Components: []component.Component{comp}})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthIteration, err)
							return
						}
					// Set a new APM config to check that we update correctly
					case 3:
						// In the previous iteration, the (fake) component has received another CheckinExpected
						// message to propagate a modified APM configuration. In this iteration we are about to
						// retrieve the APM configuration from the same component via the retrieve_apm_config
						// action.
						assert.Eventuallyf(t, func() bool {
							// check the component
							res, err := m.PerformAction(
								context.Background(),
								comp,
								comp.Units[0],
								fakecmp.ActionRetrieveAPMConfig,
								nil)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to PerformAction %s: %w",
									healthIteration, fakecmp.ActionRetrieveAPMConfig, err)
								return false
							}

							retrievedApmConfig, err = extractAPMConfigFromActionResult(t, res)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to retrieve APM Config from ActionResult %s: %w",
									healthIteration, fakecmp.ActionRetrieveAPMConfig, err)
								return false
							}

							return gproto.Equal(modifiedAPMConfig, retrievedApmConfig)
						}, 1*time.Second, 100*time.Millisecond, "APM config was not received by component. expected: %s actual: %s", modifiedAPMConfig, retrievedApmConfig)

						comp.Component = &proto.Component{
							ApmConfig: nil,
						}
						m.Update(component.Model{Components: []component.Component{comp}})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthIteration, err)
							return
						}

					case 4:
						// In the previous iteration, the (fake) component has received another CheckinExpected
						// message to propagate a nil APM configuration.  In this iteration we are about to
						// retrieve the APM configuration from the same component via the retrieve_apm_config
						// action.
						assert.Eventuallyf(t, func() bool {
							// check the component
							res, err := m.PerformAction(
								context.Background(),
								comp,
								comp.Units[0],
								fakecmp.ActionRetrieveAPMConfig,
								nil)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to PerformAction %s: %w",
									healthIteration, fakecmp.ActionRetrieveAPMConfig, err)
								return false
							}

							retrievedApmConfig, err = extractAPMConfigFromActionResult(t, res)
							if err != nil {
								subscriptionErrCh <- fmt.Errorf("[case %d]: failed to retrieve APM Config from ActionResult %s: %w",
									healthIteration, fakecmp.ActionRetrieveAPMConfig, err)
								return false
							}
							return retrievedApmConfig == nil
						}, 1*time.Second, 100*time.Millisecond, "APM config was not received by component. expected: nil actual: %s", retrievedApmConfig)

						doneCh <- struct{}{}
					}

				case client.UnitStateStarting:
					// acceptable

				case client.UnitStateConfiguring:
					// set unit back to healthy, so other cases will run.
					comp.Units[0].Config = component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy",
					})

					m.Update(component.Model{Components: []component.Component{comp}})
					err := <-m.errCh
					if err != nil {
						t.Logf("error updating component state to health: %v", err)

						subscriptionErrCh <- fmt.Errorf("failed to update component: %w", err)
					}

				default:
					// unexpected state that should not have occurred
					subscriptionErrCh <- fmt.Errorf("unit reported unexpected state: %v",
						unit.State)
				}

			}
		}
	}()

	defer drainErrChan(managerErrCh)
	defer drainErrChan(subscriptionErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	timeout := 30 * time.Second
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	// Wait for a success, an error or time out
	for {
		select {
		case <-timeoutTimer.C:
			t.Fatalf("timed out after %s", timeout)
		case err := <-managerErrCh:
			require.NoError(t, err)
		case err := <-subscriptionErrCh:
			require.NoError(t, err)
		case <-doneCh:
			subCancel()
			cancel()

			err = <-managerErrCh
			require.NoError(t, err)
			return
		}
	}
}

func extractAPMConfigFromActionResult(t *testing.T, res map[string]interface{}) (*proto.APMConfig, error) {
	apmCfg, ok := res["apm"]
	if !ok {
		return nil, fmt.Errorf("ActionResult for %s does not contain top level key %s", fakecmp.ActionRetrieveAPMConfig, "apm")
	}
	if apmCfg == nil {
		// the APM config is not set on the component
		return nil, nil
	}

	jsonApmConfig, ok := apmCfg.(string)
	if !ok {
		return nil, fmt.Errorf("ActionResult for %s does not contain a string value: %T", fakecmp.ActionRetrieveAPMConfig, apmCfg)
	}

	retrievedApmConfig := new(proto.APMConfig)
	err := protojson.Unmarshal([]byte(jsonApmConfig), retrievedApmConfig)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling apmconfig %s: %w", jsonApmConfig, err)
	}
	return retrievedApmConfig, nil
}

func TestManager_FakeInput_Limits(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentInfo, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		agentInfo,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig(),
	)
	require.NoError(t, err)

	managerErrCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		managerErrCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	const compID = "fake-default"
	var compMu sync.Mutex
	comp := component.Component{
		ID: compID,
		Component: &proto.Component{
			Limits: &proto.ComponentLimits{
				GoMaxProcs: 99,
			},
		},
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{},
	}

	subscriptionCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subscriptionErrCh := make(chan error)
	doneCh := make(chan struct{})

	go func() {
		sub := m.Subscribe(subscriptionCtx, compID)
		var healthyIteration int

		for {
			select {
			case <-subscriptionCtx.Done():
				return

			case componentState := <-sub.Ch():

				t.Logf("component state changed: %+v", componentState)

				switch componentState.State {
				case client.UnitStateHealthy:
					compMu.Lock()
					comp := comp // local copy for changes
					compMu.Unlock()
					healthyIteration++

					switch healthyIteration {
					// check that the initial value was set correctly
					case 1:
						assert.NotNil(t, componentState.Component)
						assert.NotNil(t, componentState.Component.Limits)
						assert.Equal(t, uint64(99), componentState.Component.Limits.GoMaxProcs)

						// then make a change and see how it's reflected on the next healthy state
						// we must replace the whole section to keep it thread-safe
						comp.Component = &proto.Component{
							Limits: &proto.ComponentLimits{
								GoMaxProcs: 101,
							},
						}
						m.Update(component.Model{
							Components: []component.Component{comp},
						})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthyIteration, err)
							return
						}
					// check if the change was handled
					case 2:
						assert.NotNil(t, componentState.Component)
						assert.NotNil(t, componentState.Component.Limits)
						assert.Equal(t, uint64(101), componentState.Component.Limits.GoMaxProcs)

						comp.Component = nil
						m.Update(component.Model{
							Components: []component.Component{comp},
						})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthyIteration, err)
							return
						}
					// check if the empty config is handled
					case 3:
						assert.Nil(t, componentState.Component)
						doneCh <- struct{}{}
					}
				// allowed states
				case client.UnitStateStarting:
				case client.UnitStateConfiguring:
				default:
					// unexpected state that should not have occurred
					subscriptionErrCh <- fmt.Errorf("unit reported unexpected state: %v",
						componentState.State)
				}
			}
		}
	}()

	defer drainErrChan(managerErrCh)
	defer drainErrChan(subscriptionErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	timeout := 30 * time.Second
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	// Wait for a success, an error or time out
	for {
		select {
		case <-timeoutTimer.C:
			t.Fatalf("timed out after %s", timeout)
		case err := <-managerErrCh:
			require.NoError(t, err)
		case err := <-subscriptionErrCh:
			require.NoError(t, err)
		case <-doneCh:
			subCancel()
			cancel()

			err = <-managerErrCh
			require.NoError(t, err)
			return
		}
	}
}

func TestManager_FakeShipper_Limits(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentInfo, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		agentInfo,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig(),
	)
	require.NoError(t, err)

	managerErrCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		managerErrCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	shipperPath := testBinary(t, "shipper")
	const compID = "fake-shipper-default"
	var compMu sync.Mutex
	comp := component.Component{
		ID: compID,
		ShipperSpec: &component.ShipperRuntimeSpec{
			ShipperType: "fake-shipper",
			BinaryName:  "",
			BinaryPath:  shipperPath,
			Spec:        fakeShipperSpec,
		},
		Component: &proto.Component{
			Limits: &proto.ComponentLimits{
				GoMaxProcs: 99,
			},
		},
		Units: []component.Unit{},
	}

	subscriptionCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subscriptionErrCh := make(chan error)
	doneCh := make(chan struct{})

	go func() {
		sub := m.Subscribe(subscriptionCtx, compID)
		var healthyIteration int

		for {
			select {
			case <-subscriptionCtx.Done():
				return

			case componentState := <-sub.Ch():

				t.Logf("component state changed: %+v", componentState)

				switch componentState.State {
				case client.UnitStateHealthy:
					compMu.Lock()
					comp := comp // local copy for changes
					compMu.Unlock()
					healthyIteration++

					switch healthyIteration {
					// check that the initial value was set correctly
					case 1:
						assert.NotNil(t, componentState.Component)
						assert.NotNil(t, componentState.Component.Limits)
						assert.Equal(t, uint64(99), componentState.Component.Limits.GoMaxProcs)

						// then make a change and see how it's reflected on the next healthy state
						// we must replace the whole section to keep it thread-safe
						comp.Component = &proto.Component{
							Limits: &proto.ComponentLimits{
								GoMaxProcs: 101,
							},
						}
						m.Update(component.Model{
							Components: []component.Component{comp},
						})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthyIteration, err)
							return
						}
					// check if the change was handled
					case 2:
						assert.NotNil(t, componentState.Component)
						assert.NotNil(t, componentState.Component.Limits)
						assert.Equal(t, uint64(101), componentState.Component.Limits.GoMaxProcs)

						comp.Component = nil
						m.Update(component.Model{
							Components: []component.Component{comp},
						})
						err := <-m.errCh
						if err != nil {
							subscriptionErrCh <- fmt.Errorf("[case %d]: failed to update component: %w",
								healthyIteration, err)
							return
						}
					// check if the empty config is handled
					case 3:
						assert.Nil(t, componentState.Component)
						doneCh <- struct{}{}
					}
				// allowed states
				case client.UnitStateStarting:
				case client.UnitStateConfiguring:
				default:
					// unexpected state that should not have occurred
					subscriptionErrCh <- fmt.Errorf("unit reported unexpected state: %v",
						componentState.State)
				}
			}
		}
	}()

	defer drainErrChan(managerErrCh)
	defer drainErrChan(subscriptionErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	timeout := 30 * time.Second
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	// Wait for a success, an error or time out
	for {
		select {
		case <-timeoutTimer.C:
			t.Fatalf("timed out after %s", timeout)
		case err := <-managerErrCh:
			require.NoError(t, err)
		case err := <-subscriptionErrCh:
			require.NoError(t, err)
		case <-doneCh:
			subCancel()
			cancel()

			err = <-managerErrCh
			require.NoError(t, err)
			return
		}
	}
}

func TestManager_FakeInput_BadUnitToGood(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
			{
				ID:   "bad-input",
				Type: client.UnitTypeInput,
				Err:  errors.New("hard-error for config"),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		unitBad := true

		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							// update the bad unit to be good; so it will transition to healthy
							updatedComp := comp
							updatedComp.Units = make([]component.Unit, len(comp.Units))
							copy(updatedComp.Units, comp.Units)
							updatedComp.Units[1] = component.Unit{
								ID:       "bad-input",
								Type:     client.UnitTypeInput,
								LogLevel: client.UnitLogLevelTrace,
								Config: component.MustExpectedConfig(map[string]interface{}{
									"type":    "fake",
									"state":   int(client.UnitStateHealthy),
									"message": "Fake Healthy 2",
								}),
							}

							unitBad = false
							m.Update(component.Model{Components: []component.Component{updatedComp}})
							err := <-m.errCh
							if err != nil {
								subErrCh <- err
							}
						} else if unit.State == client.UnitStateStopped || unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
					unit, ok = state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "bad-input"}]
					if ok {
						if unitBad {
							if unit.State != client.UnitStateFailed {
								subErrCh <- errors.New("bad-input unit should be failed")
							}
						} else {
							if unit.State == client.UnitStateFailed {
								if unit.Message == "hard-error for config" {
									// still hard-error; wait for it to go healthy
								} else {
									subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
								}
							} else if unit.State == client.UnitStateHealthy {
								// bad unit is now healthy; stop the component
								m.Update(component.Model{Components: []component.Component{}})
								err := <-m.errCh
								if err != nil {
									subErrCh <- err
								}
							} else if unit.State == client.UnitStateStopped {
								subErrCh <- nil
							} else if unit.State == client.UnitStateStarting {
								// acceptable
							} else {
								// unknown state that should not have occurred
								subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
							}
						}
					} else {
						subErrCh <- errors.New("unit missing: bad-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_GoodUnitToBad(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	runResultChan := make(chan error, 1)
	go func() {
		runResultChan <- m.Run(ctx)
	}()

	binaryPath := testBinary(t, "component")
	healthyComp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
			{
				ID:       "good-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Health 2",
				}),
			},
		},
	}
	// unhealthyComp is a copy of healthyComp with an error inserted in the
	// second unit
	unhealthyComp := healthyComp
	unhealthyComp.Units = make([]component.Unit, len(healthyComp.Units))
	copy(unhealthyComp.Units, healthyComp.Units)
	unhealthyComp.Units[1] = component.Unit{
		ID:   "good-input",
		Type: client.UnitTypeInput,
		Err:  errors.New("hard-error for config"),
	}
	goodUnitKey := ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "good-input"}

	// Wait for Manager to start up
	timedWaitForReady(t, m, 1*time.Second)

	sub := m.Subscribe(ctx, "fake-default")

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()

	m.Update(component.Model{Components: []component.Component{healthyComp}})
	err = <-m.errCh
	require.NoError(t, err)

	// nextState tracks the stage of the test. We expect the sequence
	// Starting -> Healthy -> Failed -> Stopped.
	nextState := client.UnitStateHealthy

LOOP:
	for {
		var state ComponentState
		select {
		case <-endTimer.C:
			require.Fail(t, "timed out waiting for component state update")
		case state = <-sub.Ch():
			t.Logf("component state changed: %+v", state)
		}

		require.NotEqual(t, client.UnitStateFailed, state.State, "component should not fail")
		unit, ok := state.Units[goodUnitKey]
		require.True(t, ok, "unit good-input must be present")

		if nextState == client.UnitStateHealthy {
			// Waiting for unit to become healthy, if it's still starting skip
			// to the next update
			if unit.State == client.UnitStateStarting {
				continue LOOP
			}
			if unit.State == client.UnitStateHealthy {
				// good unit is healthy; now make it bad
				t.Logf("marking good-input as having a hard-error for config")
				m.Update(component.Model{Components: []component.Component{unhealthyComp}})
				err := <-m.errCh
				require.NoError(t, err, "Component model update should succeed")

				// We next expect to transition to Failed
				nextState = client.UnitStateFailed
			} else {
				// Unit should only be starting or healthy in this stage,
				// anything else is an error.
				require.FailNowf(t, "Incorrect state", "Expected STARTING or HEALTHY, got %v", unit.State)
			}
		} else if nextState == client.UnitStateFailed {
			// Waiting for unit to fail, if it's still healthy skip to the next
			// update
			if unit.State == client.UnitStateHealthy {
				continue LOOP
			}
			if unit.State == client.UnitStateFailed {
				// Reached the expected state, now send an empty component model
				// to stop everything.
				m.Update(component.Model{Components: []component.Component{}})
				err := <-m.errCh
				require.NoError(t, err, "Component model update should succeed")
				nextState = client.UnitStateStopped
			} else {
				// Unit should only be healthy or failed in this stage, anything
				// else is an error.
				require.FailNow(t, "Incorrect state", "Expected HEALTHY or FAILED, got %v", unit.State)
			}
		} else if nextState == client.UnitStateStopped {
			// Waiting for component to stop, if it's still Failed skip to
			// the next update
			if unit.State == client.UnitStateFailed {
				continue LOOP
			}
			if unit.State == client.UnitStateStopped {
				// Success, we've finished the whole sequence
				break LOOP
			} else {
				// Unit should only be failed or stopped in this stage, anything
				// else is an error.
				require.FailNowf(t, "Incorrect state", "Expected FAILED or STOPPED, got %v", unit.State)
			}
		}
	}

	cancel()
	err = <-runResultChan
	require.Equal(t, context.Canceled, err, "Run should return with context canceled, got %v", err.Error())
}

func TestManager_FakeInput_NoDeadlock(t *testing.T) {
	// NOTE: This is a long-running test that spams the runtime managers `Update` function to try and
	// trigger a deadlock. This test takes 2 minutes to run trying to re-produce issue:
	// https://github.com/elastic/elastic-agent/issues/2691

	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelError, // test log will get spammed with the constant updates (error to prevent spam)
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	updatedCh := make(chan time.Time)
	updatedErr := make(chan error)
	updatedCtx, updatedCancel := context.WithCancel(context.Background())
	defer updatedCancel()
	go func() {
		// spam update on component trying to cause a deadlock
		comp := comp
		i := 0
		for {
			if updatedCtx.Err() != nil {
				return
			}
			updatedComp := comp
			updatedComp.Units = make([]component.Unit, 1)
			updatedComp.Units[0] = component.Unit{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelError, // test log will get spammed with the constant updates (error to prevent spam)
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": fmt.Sprintf("Fake Healthy %d", i),
				}),
			}
			i += 1
			comp = updatedComp
			m.Update(component.Model{Components: []component.Component{updatedComp}})
			err := <-m.errCh
			if err != nil {
				updatedErr <- err
				return
			}
			updatedCh <- time.Now()
		}
	}()

	deadlockErr := make(chan error)
	go func() {
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-updatedCtx.Done():
				return
			case <-updatedCh:
				// update did occur
				t.Reset(15 * time.Second)
			case <-t.C:
				// timeout hit waiting for another update to work
				deadlockErr <- errors.New("hit deadlock")
				return
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(updatedErr)
	defer drainErrChan(deadlockErr)

	// wait 2 minutes for a deadlock to occur
	endTimer := time.NewTimer(2 * time.Minute)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			// no deadlock after timeout (all good stop the component)
			updatedCancel()
			m.Update(component.Model{Components: []component.Component{}})
			<-m.errCh // Don't care about the result of Update, just that it runs
			break LOOP
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-updatedErr:
			require.NoError(t, err)
			break LOOP
		case err := <-deadlockErr:
			require.NoError(t, err)
			break LOOP
		}
	}

	updatedCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_Configure(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							// update config to change the state to degraded
							comp.Units[0].Config = component.MustExpectedConfig(map[string]interface{}{
								"type":    "fake",
								"state":   int(client.UnitStateDegraded),
								"message": "Fake Degraded",
							})
							m.Update(component.Model{Components: []component.Component{comp}})
							err := <-m.errCh
							if err != nil {
								subErrCh <- err
							}
						} else if unit.State == client.UnitStateDegraded {
							subErrCh <- nil
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_RemoveUnit(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input-0",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy 0",
				}),
			},
			{
				ID:       "fake-input-1",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy 1",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		unit1Stopped := false

		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit0, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input-0"}]
					if ok {
						if unit0.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit 0 failed: %s", unit0.Message)
						} else if unit0.State == client.UnitStateStarting || unit0.State == client.UnitStateHealthy {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit 0 reported unexpected state: %v", unit0.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input-0")
					}
					unit1, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input-1"}]
					if ok {
						if unit1.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit 1 failed: %s", unit1.Message)
						} else if unit1.State == client.UnitStateHealthy {
							// unit1 is healthy lets remove it from the component
							comp.Units = comp.Units[0:1]
							m.Update(component.Model{Components: []component.Component{comp}})
							err = <-m.errCh
							if err != nil {
								subErrCh <- err
							}
						} else if unit1.State == client.UnitStateStarting || unit1.State == client.UnitStateStopping {
							// acceptable
						} else if unit1.State == client.UnitStateStopped {
							// unit should have been reported stopped before being removed
							unit1Stopped = true
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit 1 reported unexpected state: %v", unit1.State)
						}
					} else {
						if len(comp.Units) == 1 {
							if unit1Stopped {
								// unit reported stopped then removed (perfect!)
								subErrCh <- nil
							} else {
								// never reported stopped
								subErrCh <- errors.New("unit 1 removed but not reported stop first")
							}
						} else {
							// should not be removed
							subErrCh <- errors.New("unit missing: fake-input-1")
						}
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_ActionState(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							// must be called in a separate go routine because it cannot block receiving from the
							// subscription channel
							go func() {
								actionCtx, actionCancel := context.WithTimeout(context.Background(), 15*time.Second)
								_, err := m.PerformAction(actionCtx, comp, comp.Units[0], "set_state", map[string]interface{}{
									"state":   int(client.UnitStateDegraded),
									"message": "Action Set Degraded",
								})
								actionCancel()
								if err != nil {
									subErrCh <- err
								}
							}()
						} else if unit.State == client.UnitStateDegraded {
							// action set it to degraded
							subErrCh <- nil
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_Restarts(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		killed := false

		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					if !killed {
						subErrCh <- fmt.Errorf("component failed: %s", state.Message)
					}
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							if !killed {
								subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
							}
						} else if unit.State == client.UnitStateHealthy {
							// force the input to exit and it should be restarted
							if !killed {
								killed = true

								t.Log("triggering kill through action")
								actionCtx, actionCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
								_, err := m.PerformAction(actionCtx, comp, comp.Units[0], "kill", nil)
								actionCancel()
								if !errors.Is(err, context.DeadlineExceeded) {
									// should have got deadline exceeded for this call
									if err == nil {
										err = fmt.Errorf("should have got deadline exceeded")
									} else {
										err = fmt.Errorf("should have got deadline exceeded, instead got: %w", err)
									}
									subErrCh <- err
								}
							} else {
								// got back to healthy after kill
								subErrCh <- nil
							}
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_Restarts_ConfigKill(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	// adjust input spec to allow restart
	cmdSpec := *fakeInputSpec.Command
	cmdSpec.RestartMonitoringPeriod = 1 * time.Second
	cmdSpec.MaxRestartsPerPeriod = 10
	inputSpec := fakeInputSpec
	inputSpec.Command = &cmdSpec

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       inputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		killed := false

		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					if !killed {
						subErrCh <- fmt.Errorf("component failed: %s", state.Message)
					}
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							if !killed {
								subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
							}
						} else if unit.State == client.UnitStateHealthy {
							// force the input to exit and it should be restarted
							if !killed {
								killed = true

								r := regexp.MustCompile(`pid \'(?P<pid>\d+)\'`)
								rp := r.FindStringSubmatch(state.Message)
								t.Logf("triggering kill through config on pid %s", rp)
								comp.Units[0].Config = component.MustExpectedConfig(map[string]interface{}{
									"type":    "fake",
									"state":   int(client.UnitStateHealthy),
									"message": "Fake Healthy",
									"kill":    rp[1],
								})
								m.Update(component.Model{Components: []component.Component{comp}})
								err := <-m.errCh
								if err != nil {
									subErrCh <- err
								}
							} else {
								// got back to healthy after kill
								subErrCh <- nil
							}
						} else if unit.State == client.UnitStateStarting || unit.State == client.UnitStateStopped {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(1 * time.Minute)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 1 minute")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_KeepsRestarting(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	// adjust input spec to allow restart
	cmdSpec := *fakeInputSpec.Command
	cmdSpec.RestartMonitoringPeriod = 1 * time.Second
	cmdSpec.MaxRestartsPerPeriod = 10
	inputSpec := fakeInputSpec
	inputSpec.Command = &cmdSpec

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       inputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelTrace,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":             "fake",
					"state":            int(client.UnitStateHealthy),
					"message":          "Fake Healthy",
					"kill_on_interval": true,
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		lastStoppedCount := 0
		stoppedCount := 0

		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					// should not go failed because we allow restart per period
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							// unit should not be failed because we allow restart per period
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							if lastStoppedCount != stoppedCount {
								lastStoppedCount = stoppedCount

								// send new config on each healthy report
								comp.Units[0].Config = component.MustExpectedConfig(map[string]interface{}{
									"type":             "fake",
									"state":            int(client.UnitStateHealthy),
									"message":          fmt.Sprintf("Fake Healthy %d", lastStoppedCount),
									"kill_on_interval": true,
								})
								m.Update(component.Model{Components: []component.Component{comp}})
								err := <-m.errCh
								if err != nil {
									subErrCh <- err
								}
							}
							if stoppedCount >= 3 {
								// got stopped 3 times and got back to healthy
								subErrCh <- nil
							}
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else if unit.State == client.UnitStateStopped {
							stoppedCount += 1
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(1 * time.Minute)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 1 minute")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_RestartsOnMissedCheckins(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec: component.InputSpec{
				Name: "fake",
				Command: &component.CommandSpec{
					Timeouts: component.CommandTimeoutSpec{
						// very low checkin timeout so we can cause missed check-ins
						Checkin: 100 * time.Millisecond,
						Restart: 10 * time.Second,
						Stop:    30 * time.Second,
					},
				},
			},
		},
		Units: []component.Unit{
			{
				ID:   "fake-input",
				Type: client.UnitTypeInput,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		wasDegraded := false

		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)

				switch state.State {
				case client.UnitStateStarting:
				case client.UnitStateHealthy:
					// starting and healthy are allowed
				case client.UnitStateDegraded:
					// should go to degraded first
					wasDegraded = true
				case client.UnitStateFailed:
					if wasDegraded {
						subErrCh <- nil
					} else {
						subErrCh <- errors.New("should have been degraded before failed")
					}
				default:
					subErrCh <- fmt.Errorf("unknown component state: %v", state.State)
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_InvalidAction(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:   "fake-input",
				Type: client.UnitTypeInput,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							actionCtx, actionCancel := context.WithTimeout(context.Background(), 5*time.Second)
							_, err := m.PerformAction(actionCtx, comp, comp.Units[0], "invalid_missing_action", nil)
							actionCancel()
							if err == nil {
								subErrCh <- fmt.Errorf("should have returned an error")
							} else if err.Error() != errActionUndefined {
								subErrCh <- fmt.Errorf("should have returned error: action undefined")
							} else {
								subErrCh <- nil
							}
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_MultiComponent(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentInfo, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		agentInfo,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig())
	require.NoError(t, err)

	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	runtimeSpec := component.InputRuntimeSpec{
		InputType:  "fake",
		BinaryName: "",
		BinaryPath: binaryPath,
		Spec:       fakeInputSpec,
	}
	components := []component.Component{
		{
			ID:        "fake-0",
			InputSpec: &runtimeSpec,
			Units: []component.Unit{
				{
					ID:   "fake-input-0-0",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-0",
					}),
				},
				{
					ID:   "fake-input-0-1",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-1",
					}),
				},
				{
					ID:   "fake-input-0-2",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-2",
					}),
				},
			},
		},
		{
			ID:        "fake-1",
			InputSpec: &runtimeSpec,
			Units: []component.Unit{
				{
					ID:   "fake-input-1-0",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 1-0",
					}),
				},
				{
					ID:   "fake-input-1-1",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 1-1",
					}),
				},
				{
					ID:   "fake-input-1-2",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 1-2",
					}),
				},
			},
		},
		{
			ID:        "fake-2",
			InputSpec: &runtimeSpec,
			Units: []component.Unit{
				{
					ID:   "fake-input-2-0",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 2-0",
					}),
				},
				{
					ID:   "fake-input-2-1",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 2-1",
					}),
				},
				{
					ID:   "fake-input-2-2",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 2-2",
					}),
				},
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh0 := make(chan error)
	subErrCh1 := make(chan error)
	subErrCh2 := make(chan error)
	go func() {
		sub0 := m.Subscribe(subCtx, "fake-0")
		sub1 := m.Subscribe(subCtx, "fake-1")
		sub2 := m.Subscribe(subCtx, "fake-2")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub0.Ch():
				t.Logf("component fake-0 state changed: %+v", state)
				signalState(subErrCh0, &state, []client.UnitState{client.UnitStateHealthy})
			case state := <-sub1.Ch():
				t.Logf("component fake-1 state changed: %+v", state)
				signalState(subErrCh1, &state, []client.UnitState{client.UnitStateHealthy})
			case state := <-sub2.Ch():
				t.Logf("component fake-2 state changed: %+v", state)
				signalState(subErrCh2, &state, []client.UnitState{client.UnitStateHealthy})
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh0)
	defer drainErrChan(subErrCh1)
	defer drainErrChan(subErrCh2)

	m.Update(component.Model{Components: components})
	err = <-m.errCh
	require.NoError(t, err)

	count := 0
	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh0:
			require.NoError(t, err)
			count++
			if count >= 3 {
				break LOOP
			}
		case err := <-subErrCh1:
			require.NoError(t, err)
			count++
			if count >= 3 {
				break LOOP
			}
		case err := <-subErrCh2:
			require.NoError(t, err)
			count++
			if count >= 3 {
				break LOOP
			}
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_LogLevel(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		ai,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig())
	require.NoError(t, err)

	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	comp := component.Component{
		ID: "fake-default",
		InputSpec: &component.InputRuntimeSpec{
			InputType:  "fake",
			BinaryName: "",
			BinaryPath: binaryPath,
			Spec:       fakeInputSpec,
		},
		Units: []component.Unit{
			{
				ID:       "fake-input",
				Type:     client.UnitTypeInput,
				LogLevel: client.UnitLogLevelInfo,
				Config: component.MustExpectedConfig(map[string]interface{}{
					"type":    "fake",
					"state":   int(client.UnitStateHealthy),
					"message": "Fake Healthy",
				}),
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		sub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-sub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-input"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							updatedComp := comp
							updatedComp.Units = make([]component.Unit, len(comp.Units))
							copy(updatedComp.Units, comp.Units)
							updatedComp.Units[0] = component.Unit{
								ID:       "fake-input",
								Type:     client.UnitTypeInput,
								LogLevel: client.UnitLogLevelTrace,
								Config: component.MustExpectedConfig(map[string]interface{}{
									"type":    "fake",
									"state":   int(client.UnitStateHealthy),
									"message": "Fake Healthy",
								}),
							}

							actionCtx, actionCancel := context.WithTimeout(context.Background(), 5*time.Second)
							_, err := m.PerformAction(actionCtx, comp, comp.Units[0], "invalid_missing_action", nil)
							actionCancel()
							if err == nil {
								subErrCh <- fmt.Errorf("should have returned an error")
							} else if err.Error() != errActionUndefined {
								subErrCh <- fmt.Errorf("should have returned error: action undefined")
							} else {
								subErrCh <- nil
							}
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: []component.Component{comp}})
	err = <-m.errCh
	require.NoError(t, err)

	endTimer := time.NewTimer(30 * time.Second)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after 30 seconds")
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeShipper(t *testing.T) {
	/*
		This test runs one instance of the fake/component and an instance of the fake/shipper. They get connected
		together, and it ensures that a test event is sent between each instance. Below is a breakdown on how this
		test performs this work and ensures that an event is sent between the two instances.

		1. Wait for the shipper input (GRPC server) is healthy.
		2. Wait for the component output (GRPC client) is healthy.
		3. Create a unique ID to use for the event ID.
		4. Send `record_event` action to the shipper input (GRPC server); won't return until it actually gets the event.
		5. Send `send_event` action to the component fake input (GRPC client); returns once sent.
		6. Wait for `record_event` action to return from the shipper input (GRPC server).
	*/
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(newDebugLogger(t), newDebugLogger(t), "localhost:0", ai, apmtest.DiscardTracer, newTestMonitoringMgr(), configuration.DefaultGRPCConfig())
	require.NoError(t, err)
	errCh := make(chan error)
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	componentPath := testBinary(t, "component")
	shipperPath := testBinary(t, "shipper")
	comps := []component.Component{
		{
			ID: "fake-default",
			InputSpec: &component.InputRuntimeSpec{
				InputType:  "fake",
				BinaryName: "",
				BinaryPath: componentPath,
				Spec:       fakeInputSpec,
			},
			Units: []component.Unit{
				{
					ID:       "fake-input",
					Type:     client.UnitTypeInput,
					LogLevel: client.UnitLogLevelTrace,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy",
					}),
				},
				{
					ID:       "fake-default",
					Type:     client.UnitTypeOutput,
					LogLevel: client.UnitLogLevelTrace,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type": "fake-shipper",
					}),
				},
			},
			ShipperRef: &component.ShipperReference{
				ComponentID: "fake-shipper-default",
				UnitID:      "fake-default",
			},
		},
		{
			ID: "fake-shipper-default",
			ShipperSpec: &component.ShipperRuntimeSpec{
				ShipperType: "fake-shipper",
				BinaryName:  "",
				BinaryPath:  shipperPath,
				Spec:        fakeShipperSpec,
			},
			Units: []component.Unit{
				{
					ID:       "fake-default",
					Type:     client.UnitTypeInput,
					LogLevel: client.UnitLogLevelTrace,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"id":   "fake-default",
						"type": "fake-shipper",
						"units": []interface{}{
							map[string]interface{}{
								"id": "fake-input",
								"config": map[string]interface{}{
									"type":    "fake",
									"state":   int(client.UnitStateHealthy),
									"message": "Fake Healthy",
								},
							},
						},
					}),
				},
				{
					ID:       "fake-default",
					Type:     client.UnitTypeOutput,
					LogLevel: client.UnitLogLevelTrace,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type": "fake-action-output",
					}),
				},
			},
		},
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	subErrCh := make(chan error)
	go func() {
		shipperInputOn := false
		shipperOutputOn := false
		compConnected := false
		eventSent := false

		sendEvent := func() (bool, error) {
			if !shipperInputOn || !shipperOutputOn || !compConnected {
				// wait until connected
				return false, nil
			}
			if eventSent {
				// other path already sent event
				return false, nil
			}
			eventSent = true

			// send an event between component and the fake shipper
			eventID, err := uuid.NewV4()
			if err != nil {
				return true, err
			}

			// wait for the event on the shipper side
			gotEvt := make(chan error)
			go func() {
				actionCtx, actionCancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := m.PerformAction(actionCtx, comps[1], comps[1].Units[1], "record_event", map[string]interface{}{
					"id": eventID.String(),
				})
				actionCancel()
				gotEvt <- err
			}()

			// send the fake event
			actionCtx, actionCancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err = m.PerformAction(actionCtx, comps[0], comps[0].Units[0], "send_event", map[string]interface{}{
				"id": eventID.String(),
			})
			actionCancel()
			if err != nil {
				return true, err
			}

			err = <-gotEvt
			if err == nil {
				t.Logf("successfully sent event from fake input to fake shipper, event ID: %s", eventID.String())
			}
			return true, err
		}

		shipperSub := m.Subscribe(subCtx, "fake-shipper-default")
		compSub := m.Subscribe(subCtx, "fake-default")
		for {
			select {
			case <-subCtx.Done():
				return
			case state := <-shipperSub.Ch():
				t.Logf("shipper state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("shipper failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeInput, UnitID: "fake-default"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							shipperInputOn = true
							ok, err := sendEvent()
							if ok {
								if err != nil {
									subErrCh <- err
								} else {
									// successful; turn it all off
									m.Update(component.Model{Components: []component.Component{}})
									err = <-m.errCh
									if err != nil {
										subErrCh <- err
									}
								}
							}
						} else if unit.State == client.UnitStateStopped {
							subErrCh <- nil
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("input unit missing: fake-default")
					}
					unit, ok = state.Units[ComponentUnitKey{UnitType: client.UnitTypeOutput, UnitID: "fake-default"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							shipperOutputOn = true
							ok, err := sendEvent()
							if ok {
								if err != nil {
									subErrCh <- err
								} else {
									// successful; turn it all off
									m.Update(component.Model{Components: []component.Component{}})
									err := <-m.errCh
									if err != nil {
										subErrCh <- err
									}
								}
							}
						} else if unit.State == client.UnitStateStopped {
							subErrCh <- nil
						} else if unit.State == client.UnitStateStarting {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("output unit missing: fake-default")
					}
				}
			case state := <-compSub.Ch():
				t.Logf("component state changed: %+v", state)
				if state.State == client.UnitStateFailed {
					subErrCh <- fmt.Errorf("component failed: %s", state.Message)
				} else {
					unit, ok := state.Units[ComponentUnitKey{UnitType: client.UnitTypeOutput, UnitID: "fake-default"}]
					if ok {
						if unit.State == client.UnitStateFailed {
							subErrCh <- fmt.Errorf("unit failed: %s", unit.Message)
						} else if unit.State == client.UnitStateHealthy {
							compConnected = true
							ok, err := sendEvent()
							if ok {
								if err != nil {
									subErrCh <- err
								} else {
									// successful; turn it all off
									m.Update(component.Model{Components: []component.Component{}})
									err := <-m.errCh
									if err != nil {
										subErrCh <- err
									}
								}
							}
						} else if unit.State == client.UnitStateStopped {
							subErrCh <- nil
						} else if unit.State == client.UnitStateStarting || unit.State == client.UnitStateConfiguring {
							// acceptable
						} else {
							// unknown state that should not have occurred
							subErrCh <- fmt.Errorf("unit reported unexpected state: %v", unit.State)
						}
					} else {
						subErrCh <- errors.New("unit missing: fake-input")
					}
				}
			}
		}
	}()

	defer drainErrChan(errCh)
	defer drainErrChan(subErrCh)

	m.Update(component.Model{Components: comps})
	err = <-m.errCh
	require.NoError(t, err)

	timeout := 2 * time.Minute
	endTimer := time.NewTimer(timeout)
	defer endTimer.Stop()
LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after %s", timeout)
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh:
			require.NoError(t, err)
			break LOOP
		}
	}

	subCancel()
	cancel()

	err = <-errCh
	require.NoError(t, err)
}

func TestManager_FakeInput_OutputChange(t *testing.T) {
	testPaths(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ai, _ := info.NewAgentInfo(ctx, true)
	m, err := NewManager(
		newDebugLogger(t),
		newDebugLogger(t),
		"localhost:0",
		ai,
		apmtest.DiscardTracer,
		newTestMonitoringMgr(),
		configuration.DefaultGRPCConfig())
	require.NoError(t, err, "could not crete new manager")

	errCh := make(chan error)
	t.Cleanup(func() { drainErrChan(errCh) })
	go func() {
		err := m.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 1*time.Second)
	defer waitCancel()
	if err := waitForReady(waitCtx, m); err != nil {
		require.NoError(t, err)
	}

	binaryPath := testBinary(t, "component")
	runtimeSpec := component.InputRuntimeSpec{
		InputType:  "fake",
		BinaryName: "",
		BinaryPath: binaryPath,
		Spec:       fakeInputSpec,
	}
	const IDComp0 = "fake-0"
	const IDComp1 = "fake-1"

	components := []component.Component{
		{
			ID:        IDComp0,
			InputSpec: &runtimeSpec,
			Units: []component.Unit{
				{
					ID:   "fake-input-0-0",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-0",
					}),
				},
				{
					ID:   "fake-input-0-1",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-1",
					}),
				},
				{
					ID:   "fake-input-0-2",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-1",
					}),
				},
			},
		},
	}

	components2 := []component.Component{
		{
			ID:        IDComp1,
			InputSpec: &runtimeSpec,
			Units: []component.Unit{
				{
					ID:   "fake-input-1-0",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-0",
					}),
				},
				{
					ID:   "fake-input-1-1",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-1",
					}),
				},
				{
					ID:   "fake-input-1-1",
					Type: client.UnitTypeInput,
					Config: component.MustExpectedConfig(map[string]interface{}{
						"type":    "fake",
						"state":   int(client.UnitStateHealthy),
						"message": "Fake Healthy 0-1",
					}),
				},
			},
		},
	}

	type progressionStep struct {
		componentID string
		state       ComponentState
	}
	var stateProgression []progressionStep

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()

	stateProgressionCh := make(chan progressionStep)
	subErrCh0 := make(chan error)
	subErrCh1 := make(chan error)
	t.Cleanup(func() { drainErrChan(subErrCh0) })
	t.Cleanup(func() { drainErrChan(subErrCh1) })

	go func() {
		sub0 := m.Subscribe(subCtx, IDComp0)
		sub1 := m.Subscribe(subCtx, IDComp1)
		for {
			select {
			case <-subCtx.Done():
				close(stateProgressionCh)
				return
			case state := <-sub0.Ch():
				t.Logf("component %s state changed: %+v", IDComp0, state)
				signalState(
					subErrCh0,
					&state,
					[]client.UnitState{client.UnitStateHealthy, client.UnitStateStopped})
				stateProgressionCh <- progressionStep{IDComp0, state}

			case state := <-sub1.Ch():
				t.Logf("component %s state changed: %+v", IDComp1, state)
				signalState(
					subErrCh1,
					&state,
					[]client.UnitState{client.UnitStateHealthy})
				stateProgressionCh <- progressionStep{IDComp1, state}
			}
		}
	}()

	var stateProgressionWG sync.WaitGroup
	stateProgressionWG.Add(1)
	go func() {
		for step := range stateProgressionCh {
			stateProgression = append(stateProgression, step)
		}
		stateProgressionWG.Done()
	}()

	err = waitForReady(waitCtx, m)
	require.NoError(t, err, "Manager must finish initializing")

	select {
	case err := <-errCh:
		t.Fatalf("failed early: %s", err)
	default:
	}

	time.Sleep(100 * time.Millisecond)
	m.Update(component.Model{Components: components})
	err = <-m.errCh
	require.NoError(t, err)

	updateSleep := 300 * time.Millisecond
	if runtime.GOOS == component.Windows {
		// windows is slow, preventing flakiness
		updateSleep = time.Second
	}
	time.Sleep(updateSleep)
	m.Update(component.Model{Components: components2})
	err = <-m.errCh
	require.NoError(t, err)

	count := 0
	timeout := 30 * time.Second
	endTimer := time.NewTimer(timeout)
	defer endTimer.Stop()

LOOP:
	for {
		select {
		case <-endTimer.C:
			t.Fatalf("timed out after %s seconds, "+
				"did not receive enought state changes", timeout)
		case err := <-errCh:
			require.NoError(t, err)
		case err := <-subErrCh0:
			t.Logf("[subErrCh0] received: %v", err)
			require.NoError(t, err)
			count++
			if count >= 2 {
				break LOOP
			}
		case err := <-subErrCh1:
			t.Logf("[subErrCh1] received: %v", err)
			require.NoError(t, err)
			count++
			if count >= 2 {
				break LOOP
			}
		}
	}

	subCancel()
	cancel()

	// check progression, require stop fake-0 before start fake-1
	stateProgressionWG.Wait()
	comp0Stopped := false
	for _, step := range stateProgression {
		if step.componentID == IDComp0 &&
			step.state.State == client.UnitStateStopped {
			comp0Stopped = true
		}
		if step.componentID == IDComp1 &&
			step.state.State == client.UnitStateStarting {
			require.True(t, comp0Stopped)
			break
		}
	}

	err = <-errCh
	require.NoError(t, err)
}

func newDebugLogger(t *testing.T) *logger.Logger {
	t.Helper()

	loggerCfg := logger.DefaultLoggingConfig()
	loggerCfg.Level = logp.DebugLevel
	loggerCfg.ToStderr = true

	log, err := logger.NewFromConfig("", loggerCfg, false)
	require.NoError(t, err)
	return log
}

func drainErrChan(ch chan error) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func signalState(subErrCh chan error, state *ComponentState, acceptableStates []client.UnitState) {
	if state.State == client.UnitStateFailed {
		subErrCh <- fmt.Errorf("component failed: %s", state.Message)
		return
	}

	var issues []string
	healthy := 0
	for key, unit := range state.Units {
		switch {
		case unit.State == client.UnitStateStarting:
		// acceptable, but does not count as health
		case isValidState(unit.State, acceptableStates):
			healthy++
		default:

			issues = append(issues, fmt.Sprintf(
				"unit %s in invalid state %v", key.UnitID, unit.State))
		}
	}

	if len(issues) != 0 {
		subErrCh <- errors.New(strings.Join(issues, "| "))
	}

	if healthy == len(state.Units) {
		subErrCh <- nil
	}
}

func isValidState(state client.UnitState, acceptableStates []client.UnitState) bool {
	for _, s := range acceptableStates {
		if s == state {
			return true
		}
	}
	return false
}

func testPaths(t *testing.T) {
	t.Helper()

	versioned := paths.IsVersionHome()
	topPath := paths.Top()

	tmpDir, err := os.MkdirTemp("", "at-*")
	if err != nil {
		t.Fatalf("failed to create temp directory: %s", err)
	}
	paths.SetVersionHome(false)
	paths.SetTop(tmpDir)

	t.Cleanup(func() {
		paths.SetVersionHome(versioned)
		paths.SetTop(topPath)
		_ = os.RemoveAll(tmpDir)
	})
}

func testBinary(t *testing.T, name string) string {
	t.Helper()

	var err error
	binaryPath := fakeBinaryPath(name)

	binaryPath, err = filepath.Abs(binaryPath)
	if err != nil {
		t.Fatalf("failed abs %s: %s", binaryPath, err)
	}

	return binaryPath
}

func fakeBinaryPath(name string) string {
	binaryPath := filepath.Join("..", "fake", name, name)

	if runtime.GOOS == component.Windows {
		binaryPath += exeExt
	}

	return binaryPath
}

type testMonitoringManager struct{}

func newTestMonitoringMgr() *testMonitoringManager { return &testMonitoringManager{} }

func (*testMonitoringManager) EnrichArgs(_ string, _ string, args []string) []string { return args }
func (*testMonitoringManager) Prepare(_ string) error                                { return nil }
func (*testMonitoringManager) Cleanup(string) error                                  { return nil }

// waitForReady waits until the RPC server is ready to be used.
func waitForReady(ctx context.Context, m *Manager) error {
	for !m.serverReady.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

func timedWaitForReady(t *testing.T, m *Manager, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := waitForReady(ctx, m)
	if err != nil {
		require.FailNow(t, "timed out waiting for Manager to start")
	}
}
