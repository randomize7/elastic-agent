// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-agent-libs/kibana"
	"github.com/elastic/elastic-agent/pkg/control/v2/client"
	atesting "github.com/elastic/elastic-agent/pkg/testing"
	"github.com/elastic/elastic-agent/pkg/testing/define"
	"github.com/elastic/elastic-agent/pkg/testing/tools"
	"github.com/elastic/elastic-agent/pkg/testing/tools/check"
	"github.com/elastic/elastic-agent/pkg/testing/tools/estools"
	"github.com/elastic/elastic-agent/pkg/testing/tools/fleettools"
	"github.com/elastic/elastic-agent/pkg/testing/tools/testcontext"
	"github.com/elastic/elastic-transport-go/v8/elastictransport"
)

func TestLogIngestionFleetManaged(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false,
		Sudo:  true,
	})

	ctx, cancel := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cancel()

	agentFixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err)

	// 1. Create a policy in Fleet with monitoring enabled.
	// To ensure there are no conflicts with previous test runs against
	// the same ESS stack, we add the current time at the end of the policy
	// name. This policy does not contain any integration.
	t.Log("Enrolling agent in Fleet with a test policy")
	createPolicyReq := kibana.AgentPolicy{
		Name:        fmt.Sprintf("test-policy-enroll-%d", time.Now().Unix()),
		Namespace:   info.Namespace,
		Description: "test policy for agent enrollment",
		MonitoringEnabled: []kibana.MonitoringEnabledOption{
			kibana.MonitoringEnabledLogs,
			kibana.MonitoringEnabledMetrics,
		},
		AgentFeatures: []map[string]interface{}{
			{
				"name":    "test_enroll",
				"enabled": true,
			},
		},
	}

	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
	}

	// 2. Install the Elastic-Agent with the policy that
	// was just created.
	policy, err := tools.InstallAgentWithPolicy(
		ctx,
		t,
		installOpts,
		agentFixture,
		info.KibanaClient,
		createPolicyReq)
	require.NoError(t, err)
	t.Logf("created policy: %s", policy.ID)
	check.ConnectedToFleet(t, agentFixture, 5*time.Minute)

	t.Run("Monitoring logs are shipped", func(t *testing.T) {
		testMonitoringLogsAreShipped(t, ctx, info, agentFixture, policy)
	})

	t.Run("Normal logs with flattened data_stream are shipped", func(t *testing.T) {
		testFlattenedDatastreamFleetPolicy(t, ctx, info, agentFixture, policy)
	})
}

func testMonitoringLogsAreShipped(
	t *testing.T,
	ctx context.Context,
	info *define.Info,
	agentFixture *atesting.Fixture,
	policy kibana.PolicyResponse,
) {
	// Stage 1: Make sure metricbeat logs are populated
	t.Log("Making sure metricbeat logs are populated")
	docs := findESDocs(t, func() (estools.Documents, error) {
		return estools.GetLogsForDataset(info.ESClient, "elastic_agent.metricbeat")
	})
	t.Logf("metricbeat: Got %d documents", len(docs.Hits.Hits))
	require.NotZero(t, len(docs.Hits.Hits),
		"Looking for logs in dataset 'elastic_agent.metricbeat'")

	// Stage 2: make sure all components are healthy
	t.Log("Making sure all components are healthy")
	status, err := agentFixture.ExecStatus(ctx)
	require.NoError(t, err,
		"could not get agent status to verify all components are healthy")
	for _, c := range status.Components {
		assert.Equalf(t, client.Healthy, client.State(c.State),
			"component %s: want %s, got %s",
			c.Name, client.Healthy, client.State(c.State))
	}

	// Stage 3: Make sure there are no errors in logs
	t.Log("Making sure there are no error logs")
	docs = queryESDocs(t, func() (estools.Documents, error) {
		return estools.CheckForErrorsInLogs(info.ESClient, info.Namespace, []string{
			// acceptable error messages (include reason)
			"Error dialing dial tcp 127.0.0.1:9200: connect: connection refused", // beat is running default config before its config gets updated
			"Global configuration artifact is not available",                     // Endpoint: failed to load user artifact due to connectivity issues
			"Failed to download artifact",
			"Failed to initialize artifact",
			"Failed to apply initial policy from on disk configuration",
			"elastic-agent-client error: rpc error: code = Canceled desc = context canceled", // can happen on restart
			"add_cloud_metadata: received error failed requesting openstack metadata: Get \\\"https://169.254.169.254/2009-04-04/meta-data/instance-id\\\": dial tcp 169.254.169.254:443: connect: connection refused",                 // okay for the openstack metadata to not work
			"add_cloud_metadata: received error failed requesting openstack metadata: Get \\\"https://169.254.169.254/2009-04-04/meta-data/hostname\\\": dial tcp 169.254.169.254:443: connect: connection refused",                    // okay for the cloud metadata to not work
			"add_cloud_metadata: received error failed requesting openstack metadata: Get \\\"https://169.254.169.254/2009-04-04/meta-data/placement/availability-zone\\\": dial tcp 169.254.169.254:443: connect: connection refused", // okay for the cloud metadata to not work
			"add_cloud_metadata: received error failed requesting openstack metadata: Get \\\"https://169.254.169.254/2009-04-04/meta-data/instance-type\\\": dial tcp 169.254.169.254:443: connect: connection refused",               // okay for the cloud metadata to not work
			"add_cloud_metadata: received error failed with http status code 404", // okay for the cloud metadata to not work
			"add_cloud_metadata: received error failed fetching EC2 Identity Document: operation error ec2imds: GetInstanceIdentityDocument, http response error StatusCode: 404, request to EC2 IMDS failed", // okay for the cloud metadata to not work
		})
	})
	t.Logf("error logs: Got %d documents", len(docs.Hits.Hits))
	for _, doc := range docs.Hits.Hits {
		t.Logf("%#v", doc.Source)
	}
	require.Empty(t, docs.Hits.Hits)

	// Stage 4: Make sure we have message confirming central management is running
	t.Log("Making sure we have message confirming central management is running")
	docs = findESDocs(t, func() (estools.Documents, error) {
		return estools.FindMatchingLogLines(info.ESClient, info.Namespace,
			"Parsed configuration and determined agent is managed by Fleet")
	})
	require.NotZero(t, len(docs.Hits.Hits))

	// Stage 5: verify logs from the monitoring components are not sent to the output
	t.Log("Check monitoring logs")
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("could not get hostname to filter Agent: %s", err)
	}

	agentID, err := fleettools.GetAgentIDByHostname(info.KibanaClient, policy.ID, hostname)
	require.NoError(t, err, "could not get Agent ID by hostname")
	t.Logf("Agent ID: %q", agentID)

	// We cannot search for `component.id` because at the moment of writing
	// this field is not mapped. There is an issue for that:
	// https://github.com/elastic/integrations/issues/6545
	// TODO: use runtime fields while the above issue is not resolved.
	docs = findESDocs(t, func() (estools.Documents, error) {
		return estools.GetLogsForAgentID(info.ESClient, agentID)
	})
	require.NoError(t, err, "could not get logs from Agent ID: %q, err: %s",
		agentID, err)

	monRegExp := regexp.MustCompile(".*-monitoring$")
	for i, d := range docs.Hits.Hits {
		// Lazy way to navigate a map[string]any: convert to JSON then
		// decode into a struct.
		jsonData, err := json.Marshal(d.Source)
		if err != nil {
			t.Fatalf("could not encode document source as JSON: %s", err)
		}

		doc := ESDocument{}
		if err := json.Unmarshal(jsonData, &doc); err != nil {
			t.Fatalf("could not unmarshal document source: %s", err)
		}

		if monRegExp.MatchString(doc.Component.ID) {
			t.Errorf("[%d] Document on index %q with 'component.id': %q "+
				"and 'elastic_agent.id': %q. 'elastic_agent.id' must not "+
				"end in '-monitoring'\n",
				i, d.Index, doc.Component.ID, doc.ElasticAgent.ID)
		}
	}
}

// queryESDocs runs `findFn` until it returns no error. Zero documents returned
// is considered a success.
func queryESDocs(t *testing.T, findFn func() (estools.Documents, error)) estools.Documents {
	var docs estools.Documents
	require.Eventually(
		t,
		func() bool {
			var err error
			docs, err = findFn()
			if err != nil {
				t.Logf("got an error querying ES, retrying. Error: %s", err)
			}
			return err == nil
		},
		3*time.Minute,
		15*time.Second,
	)

	return docs
}

// findESDocs runs `findFn` until at least one document is returned and there is no error
func findESDocs(t *testing.T, findFn func() (estools.Documents, error)) estools.Documents {
	var docs estools.Documents
	require.Eventually(
		t,
		func() bool {
			var err error
			docs, err = findFn()
			if err != nil {
				t.Logf("got an error querying ES, retrying. Error: %s", err)
				return false
			}

			return docs.Hits.Total.Value != 0
		},
		3*time.Minute,
		15*time.Second,
	)

	return docs
}

func testFlattenedDatastreamFleetPolicy(
	t *testing.T,
	ctx context.Context,
	info *define.Info,
	agentFixture *atesting.Fixture,
	policy kibana.PolicyResponse,
) {
	dsType := "logs"
	dsNamespace := cleanString(fmt.Sprintf("%snamespace%d", t.Name(), rand.Uint64()))
	dsDataset := cleanString(fmt.Sprintf("%s-dataset", t.Name()))
	numEvents := 60

	tempDir := t.TempDir()
	logFilePath := filepath.Join(tempDir, "log.log")
	generateLogFile(t, logFilePath, 2*time.Millisecond, numEvents)

	agentFixture, err := define.NewFixture(t, define.Version())
	if err != nil {
		t.Fatalf("could not create new fixture: %s", err)
	}

	// 1. Prepare a request to add an integration to the policy
	tmpl, err := template.New(t.Name() + "custom-log-policy").Parse(policyJSON)
	if err != nil {
		t.Fatalf("cannot parse template: %s", err)
	}

	// The time here ensures there are no conflicts with the integration name
	// in Fleet.
	agentPolicyBuilder := strings.Builder{}
	err = tmpl.Execute(&agentPolicyBuilder, policyVars{
		Name:        "Log-Input-" + t.Name() + "-" + time.Now().Format(time.RFC3339),
		PolicyID:    policy.ID,
		LogFilePath: logFilePath,
		Namespace:   dsNamespace,
		Dataset:     dsDataset,
	})
	if err != nil {
		t.Fatalf("could not render template: %s", err)
	}
	// We keep a copy of the policy for debugging prurposes
	agentPolicy := agentPolicyBuilder.String()

	// 2. Call Kibana to create the policy.
	// Docs: https://www.elastic.co/guide/en/fleet/current/fleet-api-docs.html#create-integration-policy-api
	resp, err := info.KibanaClient.Connection.Send(
		http.MethodPost,
		"/api/fleet/package_policies",
		nil,
		nil,
		bytes.NewBufferString(agentPolicy))
	if err != nil {
		t.Fatalf("could not execute request to Kibana/Fleet: %s", err)
	}
	if resp.StatusCode != http.StatusOK {
		// On error dump the whole request response so we can easily spot
		// what went wrong.
		t.Errorf("received a non 200-OK when adding package to policy. "+
			"Status code: %d", resp.StatusCode)
		respDump, err := httputil.DumpResponse(resp, true)
		if err != nil {
			t.Fatalf("could not dump error response from Kibana: %s", err)
		}
		// Make debugging as easy as possible
		t.Log("================================================================================")
		t.Log("Kibana error response:")
		t.Log(string(respDump))
		t.Log("================================================================================")
		t.Log("Rendered policy:")
		t.Log(agentPolicy)
		t.Log("================================================================================")
		t.FailNow()
	}

	require.Eventually(
		t,
		ensureDocumentsInES(t, ctx, info.ESClient, dsType, dsDataset, dsNamespace, numEvents),
		120*time.Second,
		time.Second,
		"could not get all expected documents form ES")
}

// ensureDocumentsInES asserts the documents were ingested into the correct
// datastream
func ensureDocumentsInES(
	t *testing.T,
	ctx context.Context,
	esClient elastictransport.Interface,
	dsType, dsDataset, dsNamespace string,
	numEvents int,
) func() bool {

	f := func() bool {
		t.Helper()

		docs, err := estools.GetLogsForDatastream(ctx, esClient, dsType, dsDataset, dsNamespace)
		if err != nil {
			t.Logf("error quering ES, will retry later: %s", err)
		}

		if docs.Hits.Total.Value == numEvents {
			return true
		}

		return false

	}

	return f
}

// generateLogFile generates a log file by appending new lines every tick
// the lines are composed by the test name and the current time in RFC3339Nano
// This function spans a new goroutine and does not block
func generateLogFile(t *testing.T, fullPath string, tick time.Duration, events int) {
	t.Helper()
	f, err := os.Create(fullPath)
	if err != nil {
		t.Fatalf("could not create file '%s: %s", fullPath, err)
	}

	go func() {
		t.Helper()
		ticker := time.NewTicker(tick)
		t.Cleanup(ticker.Stop)

		done := make(chan struct{})
		t.Cleanup(func() { close(done) })

		defer func() {
			if err := f.Close(); err != nil {
				t.Errorf("could not close log file '%s': %s", fullPath, err)
			}
		}()

		i := 0
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				i++
				_, err := fmt.Fprintln(f, t.Name(), "Iteration: ", i, now.Format(time.RFC3339Nano))
				if err != nil {
					// The Go compiler does not allow me to call t.Fatalf from a non-test
					// goroutine, t.Errorf is our only option
					t.Errorf("could not write data to log file '%s': %s", fullPath, err)
					return
				}
				// make sure log lines are synced as quickly as possible
				if err := f.Sync(); err != nil {
					t.Errorf("could not sync file '%s': %s", fullPath, err)
				}
				if i == events {
					return
				}
			}
		}
	}()
}

func cleanString(s string) string {
	return nonAlphanumericRegex.ReplaceAllString(strings.ToLower(s), "")
}

var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9 ]+`)

var policyJSON = `
{
  "policy_id": "{{.PolicyID}}",
  "package": {
    "name": "log",
    "version": "2.3.0"
  },
  "name": "{{.Name}}",
  "namespace": "{{.Namespace}}",
  "inputs": {
    "logs-logfile": {
      "enabled": true,
      "streams": {
        "log.logs": {
          "enabled": true,
          "vars": {
            "paths": [
              "{{.LogFilePath | js}}" {{/* we need to escape windows paths */}}
            ],
            "data_stream.dataset": "{{.Dataset}}"
          }
        }
      }
    }
  }
}`

type policyVars struct {
	Name        string
	PolicyID    string
	LogFilePath string
	Namespace   string
	Dataset     string
}

type ESDocument struct {
	ElasticAgent ElasticAgent `json:"elastic_agent"`
	Component    Component    `json:"component"`
	Host         Host         `json:"host"`
}
type ElasticAgent struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Snapshot bool   `json:"snapshot"`
}
type Component struct {
	Binary string `json:"binary"`
	ID     string `json:"id"`
}
type Host struct {
	Hostname string `json:"hostname"`
}
