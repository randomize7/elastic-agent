// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package testing

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactFetcher_Name(t *testing.T) {
	f := ArtifactFetcher()
	require.Equal(t, "artifact", f.Name())
}

func TestArtifactFetcher_Default(t *testing.T) {
	f := ArtifactFetcher()
	af := f.(*artifactFetcher)
	af.doer = newFakeHttpClient()

	tmp := t.TempDir()
	res, err := f.Fetch(context.Background(), runtime.GOOS, runtime.GOARCH, "8.6.0")
	require.NoError(t, err)

	err = res.Fetch(context.Background(), t, tmp)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmp, res.Name()))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmp, res.Name()+extHash))
	require.NoError(t, err)
}

func TestArtifactFetcher_SnapshotOnly(t *testing.T) {
	f := ArtifactFetcher(WithArtifactSnapshotOnly())
	af := f.(*artifactFetcher)
	af.doer = newFakeHttpClient()

	tmp := t.TempDir()
	res, err := f.Fetch(context.Background(), runtime.GOOS, runtime.GOARCH, "8.6.0")
	require.NoError(t, err)

	err = res.Fetch(context.Background(), t, tmp)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmp, res.Name()))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmp, res.Name()+extHash))
	require.NoError(t, err)
	assert.Contains(t, res.Name(), "-SNAPSHOT")
}

type fakeHttpClient struct {
	responses []*http.Response
}

func (c *fakeHttpClient) Do(_ *http.Request) (*http.Response, error) {
	resp := c.responses[0]
	c.responses = c.responses[1:]
	return resp, nil
}

func newFakeHttpClient() *fakeHttpClient {
	artifactsResponse := `{"packages":{"elastic-agent-ironbank-8.6.0-docker-build-context.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-ironbank-8.6.0-docker-build-context.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-ironbank-8.6.0-docker-build-context.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-ironbank-8.6.0-docker-build-context.tar.gz.asc","type":"docker","classifier":"docker-build-context","attributes":{"artifactNoKpi":"true","internal":"false","url":"null/null/elastic-agent-ironbank"}},"elastic-agent-shipper-8.6.0-darwin-x86_64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-darwin-x86_64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-darwin-x86_64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-darwin-x86_64.tar.gz.asc","type":"tar","architecture":"x86_64","os":["darwin"]},"elastic-agent-cloud-8.6.0-docker-image-linux-amd64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-cloud-8.6.0-docker-image-linux-amd64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-cloud-8.6.0-docker-image-linux-amd64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-cloud-8.6.0-docker-image-linux-amd64.tar.gz.asc","type":"docker","architecture":"amd64","os":["linux"],"classifier":"docker-image","attributes":{"artifactNoKpi":"true","internal":"false","org":"beats-ci","url":"docker.elastic.co/beats-ci/elastic-agent-cloud","repo":"docker.elastic.co"}},"elastic-agent-ubi8-8.6.0-docker-image-linux-amd64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-ubi8-8.6.0-docker-image-linux-amd64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-ubi8-8.6.0-docker-image-linux-amd64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-ubi8-8.6.0-docker-image-linux-amd64.tar.gz.asc","type":"docker","architecture":"amd64","os":["linux"],"classifier":"docker-image","attributes":{"artifactNoKpi":"true","internal":"false","org":"beats","url":"docker.elastic.co/beats/elastic-agent-ubi8","repo":"docker.elastic.co"}},"elastic-agent-8.6.0-x86_64.rpm":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-x86_64.rpm","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-x86_64.rpm.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-x86_64.rpm.asc","type":"rpm","architecture":"x86_64","attributes":{"include_in_repo":"true","oss":"false"}},"elastic-agent-8.6.0-windows-x86_64.zip":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-windows-x86_64.zip","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-windows-x86_64.zip.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-windows-x86_64.zip.asc","type":"zip","architecture":"x86_64","os":["windows"]},"elastic-agent-8.6.0-arm64.deb":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-arm64.deb","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-arm64.deb.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-arm64.deb.asc","type":"deb","architecture":"arm64","attributes":{"include_in_repo":"true","oss":"false"}},"elastic-agent-shipper-8.6.0-windows-x86_64.zip":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-windows-x86_64.zip","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-windows-x86_64.zip.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-windows-x86_64.zip.asc","type":"zip","architecture":"x86_64","os":["windows"]},"elastic-agent-shipper-8.6.0-darwin-aarch64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-darwin-aarch64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-darwin-aarch64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-darwin-aarch64.tar.gz.asc","type":"tar","architecture":"aarch64","os":["darwin"]},"elastic-agent-8.6.0-docker-image-linux-arm64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-docker-image-linux-arm64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-docker-image-linux-arm64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-docker-image-linux-arm64.tar.gz.asc","type":"docker","architecture":"arm64","os":["linux"],"classifier":"docker-image","attributes":{"artifactNoKpi":"true","internal":"false","org":"beats","url":"docker.elastic.co/beats/elastic-agent","repo":"docker.elastic.co"}},"elastic-agent-8.6.0-linux-arm64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-linux-arm64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-linux-arm64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-linux-arm64.tar.gz.asc","type":"tar","architecture":"arm64","os":["linux"]},"elastic-agent-shipper-8.6.0-linux-arm64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-arm64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-arm64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-arm64.tar.gz.asc","type":"tar","architecture":"arm64","os":["linux"]},"elastic-agent-complete-8.6.0-docker-image-linux-amd64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-complete-8.6.0-docker-image-linux-amd64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-complete-8.6.0-docker-image-linux-amd64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-complete-8.6.0-docker-image-linux-amd64.tar.gz.asc","type":"docker","architecture":"amd64","os":["linux"],"classifier":"docker-image","attributes":{"artifactNoKpi":"true","internal":"false","org":"beats","url":"docker.elastic.co/beats/elastic-agent-complete","repo":"docker.elastic.co"}},"elastic-agent-8.6.0-darwin-x86_64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-darwin-x86_64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-darwin-x86_64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-darwin-x86_64.tar.gz.asc","type":"tar","architecture":"x86_64","os":["darwin"]},"elastic-agent-shipper-8.6.0-linux-x86_64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-x86_64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-x86_64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-x86_64.tar.gz.asc","type":"tar","architecture":"x86_64","os":["linux"]},"elastic-agent-8.6.0-amd64.deb":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-amd64.deb","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-amd64.deb.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-amd64.deb.asc","type":"deb","architecture":"amd64","attributes":{"include_in_repo":"true","oss":"false"}},"elastic-agent-shipper-8.6.0-windows-x86.zip":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-windows-x86.zip","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-windows-x86.zip.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-windows-x86.zip.asc","type":"zip","architecture":"x86","os":["windows"]},"elastic-agent-8.6.0-darwin-aarch64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-darwin-aarch64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-darwin-aarch64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-darwin-aarch64.tar.gz.asc","type":"tar","architecture":"aarch64","os":["darwin"]},"elastic-agent-shipper-8.6.0-linux-x86.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-x86.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-x86.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/elastic-agent-shipper/elastic-agent-shipper-8.6.0-linux-x86.tar.gz.asc","type":"tar","architecture":"x86","os":["linux"]},"elastic-agent-8.6.0-linux-x86_64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-linux-x86_64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-linux-x86_64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-linux-x86_64.tar.gz.asc","type":"tar","architecture":"x86_64","os":["linux"]},"elastic-agent-8.6.0-aarch64.rpm":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-aarch64.rpm","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-aarch64.rpm.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-aarch64.rpm.asc","type":"rpm","architecture":"aarch64","attributes":{"include_in_repo":"true","oss":"false"}},"elastic-agent-8.6.0-docker-image-linux-amd64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-docker-image-linux-amd64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-docker-image-linux-amd64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-8.6.0-docker-image-linux-amd64.tar.gz.asc","type":"docker","architecture":"amd64","os":["linux"],"classifier":"docker-image","attributes":{"artifactNoKpi":"true","internal":"false","org":"beats","url":"docker.elastic.co/beats/elastic-agent","repo":"docker.elastic.co"}},"elastic-agent-cloud-8.6.0-docker-image-linux-arm64.tar.gz":{"url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-cloud-8.6.0-docker-image-linux-arm64.tar.gz","sha_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-cloud-8.6.0-docker-image-linux-arm64.tar.gz.sha512","asc_url":"https://staging.elastic.co/8.6.0-b6c773f9/downloads/beats/elastic-agent/elastic-agent-cloud-8.6.0-docker-image-linux-arm64.tar.gz.asc","type":"docker","architecture":"arm64","os":["linux"],"classifier":"docker-image","attributes":{"artifactNoKpi":"true","internal":"false","org":"beats-ci","url":"docker.elastic.co/beats-ci/elastic-agent-cloud","repo":"docker.elastic.co"}}},"manifests":{"last-update-time":"Thu, 09 Mar 2023 20:59:39 UTC","seconds-since-last-update":89}}`
	binaryResponse := "not valid data; but its very fast to download something this small"
	hashResponse := "c2f59774022b79b61a7e6bbe28f3388d00a5bc2c7416a5c8fda79042af491d335f9b87adf905d1b154abdd2e31b200e4b1bb23cb472297596b25edef0a3b8d59"
	ascResponse := `-----BEGIN PGP SIGNATURE-----

        wsBcBAABCAAQBQJlTLh5CRD2Vuvax5DnywAAzNcIADKuYov0CMeK938JQEzR4mXP
        BoYB7Zz/IkN7A5mMztRnHi1eglr2/begM22AmC5L55OsYG5orNWV83MQPeKIr5Ub
        9gy/BktLAQTePNH6QvRzJKE3LR1pI2TT39svILoOjnPkovH/7ssa6X+/WcNE1/jX
        i7St7ZCDRZgDcmWtln7feDcYT7MdMUaQn+WP97KKbwIBTh9kOkHq9ycXnC6qT0/3
        GZT9xXTpBjctewSFja4RNCq8cmZGI2iILzFERH6MSD0iOuBV5cYKOgf/ZWtWGvad
        BuQTKP/NxCDmqhnEmJQi7BSP2UPNp+6/G8a38IyC/jlJs/f46fj+lpvQt3yn924=
        =fhCM
        -----END PGP SIGNATURE-----`
	return &fakeHttpClient{responses: []*http.Response{
		{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte(artifactsResponse))),
		},
		{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte(binaryResponse))),
		},
		{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte(hashResponse))),
		},
		{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte(ascResponse))),
		},
	}}

}
