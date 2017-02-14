package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coreos-inc/apostille/auth"
	"github.com/coreos-inc/apostille/storage"
	testUtils "github.com/coreos-inc/apostille/test"
	_ "github.com/docker/distribution/registry/auth/silly"
	"github.com/docker/notary"
	notaryStorage "github.com/docker/notary/server/storage"
	store "github.com/docker/notary/storage"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/testutils"
	tufutils "github.com/docker/notary/tuf/utils"
	"github.com/docker/notary/tuf/validation"
	"github.com/docker/notary/utils"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

func TestRunBadAddr(t *testing.T) {
	err := Run(
		context.Background(),
		Config{
			Addr:  "testAddr",
			Trust: signed.NewEd25519(),
		},
	)
	require.Error(t, err, "Passed bad addr, Run should have failed")
}

func TestRunReservedPort(t *testing.T) {
	ctx, _ := context.WithCancel(context.Background())

	err := Run(
		ctx,
		Config{
			Addr:  "localhost:80",
			Trust: signed.NewEd25519(),
		},
	)

	require.Error(t, err)
	require.IsType(t, &net.OpError{}, err)
	require.True(
		t,
		strings.Contains(err.Error(), "bind: permission denied"),
		"Received unexpected err: %s",
		err.Error(),
	)
}

func TestRepoPrefixMatches(t *testing.T) {
	gun := "quay.io/apostille"
	meta, cs, err := testutils.NewRepoMetadata(gun)
	require.NoError(t, err)
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	snChecksumBytes := sha256.Sum256(meta[data.CanonicalSnapshotRole])

	ac := auth.NewTestingAccessController("testUser")

	// successful gets
	handler := TrustMultiplexerHandler(ac, ctx, cs, nil, nil, []string{"quay.io"})
	ts := httptest.NewServer(handler)

	url := fmt.Sprintf("%s/v2/%s/_trust/tuf/", ts.URL, gun)
	uploader, err := store.NewHTTPStore(url, "", "json", "key", http.DefaultTransport)
	require.NoError(t, err)

	// uploading is cool
	require.NoError(t, uploader.SetMulti(meta))
	// getting is cool
	_, err = uploader.GetSized(data.CanonicalSnapshotRole, notary.MaxDownloadSize)
	require.NoError(t, err)

	_, err = uploader.GetSized(
		tufutils.ConsistentName(data.CanonicalSnapshotRole, snChecksumBytes[:]), notary.MaxDownloadSize)
	require.NoError(t, err)

	_, err = uploader.GetKey(data.CanonicalTimestampRole)
	require.NoError(t, err)

	// the httpstore doesn't actually delete all, so we do it manually
	req, err := http.NewRequest("DELETE", url, nil)
	require.NoError(t, err)
	res, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestRepoPrefixDoesNotMatch(t *testing.T) {
	gun := "quay.io/apostille"
	meta, cs, err := testutils.NewRepoMetadata(gun)
	require.NoError(t, err)

	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	snChecksumBytes := sha256.Sum256(meta[data.CanonicalSnapshotRole])

	ac := auth.NewTestingAccessController("testUser")

	// successful gets
	handler := TrustMultiplexerHandler(ac, ctx, cs, nil, nil, []string{"nope"})
	ts := httptest.NewServer(handler)

	url := fmt.Sprintf("%s/v2/%s/_trust/tuf/", ts.URL, gun)
	uploader, err := store.NewHTTPStore(url, "", "json", "key", http.DefaultTransport)
	require.NoError(t, err)

	require.Error(t, uploader.SetMulti(meta))

	// update the storage so we don't fail just because the metadata is missing
	for _, roleName := range data.BaseRoles {
		require.NoError(t, metaStore.UpdateCurrent(gun, notaryStorage.MetaUpdate{
			Role:    roleName,
			Data:    meta[roleName],
			Version: 1,
		}))
	}

	_, err = uploader.GetSized(data.CanonicalSnapshotRole, notary.MaxDownloadSize)
	require.Error(t, err)

	_, err = uploader.GetSized(
		tufutils.ConsistentName(data.CanonicalSnapshotRole, snChecksumBytes[:]), notary.MaxDownloadSize)
	require.Error(t, err)

	_, err = uploader.GetKey(data.CanonicalTimestampRole)
	require.Error(t, err)

	// the httpstore doesn't actually delete all, so we do it manually
	req, err := http.NewRequest("DELETE", url, nil)
	require.NoError(t, err)
	res, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestMetricsEndpoint(t *testing.T) {
	ac := auth.NewTestingAccessController("testUser")

	// successful gets
	handler := TrustMultiplexerHandler(ac, context.Background(), signed.NewEd25519(), nil, nil, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
}

// GetKeys supports only the timestamp and snapshot key endpoints
func TestGetKeysEndpoint(t *testing.T) {
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewTestingAccessController("testUser")

	// successful gets
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), nil, nil, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	rolesToStatus := map[string]int{
		data.CanonicalTimestampRole: http.StatusOK,
		data.CanonicalSnapshotRole:  http.StatusOK,
		data.CanonicalTargetsRole:   http.StatusNotFound,
		data.CanonicalRootRole:      http.StatusNotFound,
		"somerandomrole":            http.StatusNotFound,
	}

	for role, expectedStatus := range rolesToStatus {
		res, err := http.Get(
			fmt.Sprintf("%s/v2/gun/_trust/tuf/%s.key", ts.URL, role))
		require.NoError(t, err)
		require.Equal(t, expectedStatus, res.StatusCode)
	}
}

// This just checks the URL routing is working correctly and cache headers are set correctly.
// More detailed tests for this path including negative
// tests are located in /server/handlers/
func TestGetRoleByHash(t *testing.T) {
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	ts := data.SignedTimestamp{
		Signatures: make([]data.Signature, 0),
		Signed: data.Timestamp{
			SignedCommon: data.SignedCommon{
				Type:    data.TUFTypes[data.CanonicalTimestampRole],
				Version: 1,
				Expires: data.DefaultExpires(data.CanonicalTimestampRole),
			},
		},
	}
	j, err := json.Marshal(&ts)
	require.NoError(t, err)
	err = metaStore.UpdateMany("gun", []notaryStorage.MetaUpdate{{
		Role:    data.CanonicalTimestampRole,
		Version: 1,
		Data:    j,
	}})
	require.NoError(t, err)
	checksumBytes := sha256.Sum256(j)
	checksum := hex.EncodeToString(checksumBytes[:])

	// create and add a newer timestamp. We're going to try and request
	// the older version we created above.
	ts = data.SignedTimestamp{
		Signatures: make([]data.Signature, 0),
		Signed: data.Timestamp{
			SignedCommon: data.SignedCommon{
				Type:    data.TUFTypes[data.CanonicalTimestampRole],
				Version: 2,
				Expires: data.DefaultExpires(data.CanonicalTimestampRole),
			},
		},
	}
	newTS, err := json.Marshal(&ts)
	require.NoError(t, err)
	metaStore.UpdateMany("gun", []notaryStorage.MetaUpdate{{
		Role:    data.CanonicalTimestampRole,
		Version: 2,
		Data:    newTS,
	}})

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)

	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewTestingAccessController("testUser")
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), ccc, ccc, nil)
	serv := httptest.NewServer(handler)
	defer serv.Close()

	res, err := http.Get(fmt.Sprintf(
		"%s/v2/gun/_trust/tuf/%s.%s.json",
		serv.URL,
		data.CanonicalTimestampRole,
		checksum,
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	// if content is equal, checksums are guaranteed to be equal
	verifyGetResponse(t, res, j)
}

// This just checks the URL routing is working correctly and cache headers are set correctly.
// More detailed tests for this path including negative
// tests are located in /server/handlers/
func TestGetRoleByVersion(t *testing.T) {
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())

	ts := data.SignedTimestamp{
		Signatures: make([]data.Signature, 0),
		Signed: data.Timestamp{
			SignedCommon: data.SignedCommon{
				Type:    data.TUFTypes[data.CanonicalTimestampRole],
				Version: 1,
				Expires: data.DefaultExpires(data.CanonicalTimestampRole),
			},
		},
	}
	j, err := json.Marshal(&ts)
	require.NoError(t, err)
	metaStore.UpdateMany("gun", []notaryStorage.MetaUpdate{{
		Role:    data.CanonicalTimestampRole,
		Version: 1,
		Data:    j,
	}})

	// create and add a newer timestamp. We're going to try and request
	// the older version we created above.
	ts = data.SignedTimestamp{
		Signatures: make([]data.Signature, 0),
		Signed: data.Timestamp{
			SignedCommon: data.SignedCommon{
				Type:    data.TUFTypes[data.CanonicalTimestampRole],
				Version: 2,
				Expires: data.DefaultExpires(data.CanonicalTimestampRole),
			},
		},
	}
	newTS, err := json.Marshal(&ts)
	require.NoError(t, err)
	metaStore.UpdateMany("gun", []notaryStorage.MetaUpdate{{
		Role:    data.CanonicalTimestampRole,
		Version: 2,
		Data:    newTS,
	}})

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)

	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewTestingAccessController("testUser")
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), ccc, ccc, nil)
	serv := httptest.NewServer(handler)
	defer serv.Close()

	res, err := http.Get(fmt.Sprintf(
		"%s/v2/gun/_trust/tuf/%d.%s.json",
		serv.URL,
		1,
		data.CanonicalTimestampRole,
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	// if content is equal, checksums are guaranteed to be equal
	verifyGetResponse(t, res, j)
}

// This just checks the URL routing is working correctly and cache headers are set correctly.
// More detailed tests for this path including negative
// tests are located in /server/handlers/
func TestGetCurrentRole(t *testing.T) {
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	metadata, _, err := testutils.NewRepoMetadata("gun")
	require.NoError(t, err)

	// need both the snapshot and the timestamp, because when getting the current
	// timestamp the server checks to see if it's out of date (there's a new snapshot)
	// and if so, generates a new one
	metaStore.UpdateMany("gun", []notaryStorage.MetaUpdate{{
		Role:    data.CanonicalSnapshotRole,
		Version: 1,
		Data:    metadata[data.CanonicalSnapshotRole],
	}, {
		Role:    data.CanonicalTimestampRole,
		Version: 1,
		Data:    metadata[data.CanonicalTimestampRole],
	}})

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)

	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewTestingAccessController("testUser")
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), ccc, ccc, nil)
	serv := httptest.NewServer(handler)
	defer serv.Close()

	res, err := http.Get(fmt.Sprintf(
		"%s/v2/gun/_trust/tuf/%s.json",
		serv.URL,
		data.CanonicalTimestampRole,
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	verifyGetResponse(t, res, metadata[data.CanonicalTimestampRole])
}

// Verifies that the body is as expected  and that there are cache control headers
func verifyGetResponse(t *testing.T, r *http.Response, expectedBytes []byte) {
	body, err := ioutil.ReadAll(r.Body)
	require.NoError(t, err)
	require.True(t, bytes.Equal(expectedBytes, body))

	require.NotEqual(t, "", r.Header.Get("Cache-Control"))
	require.NotEqual(t, "", r.Header.Get("Last-Modified"))
	require.Equal(t, "", r.Header.Get("Pragma"))
}

// RotateKey supports only timestamp and snapshot key rotation
func TestRotateKeyEndpoint(t *testing.T) {
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewTestingAccessController("testUser")
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), ccc, ccc, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	rolesToStatus := map[string]int{
		data.CanonicalTimestampRole: http.StatusOK,
		data.CanonicalSnapshotRole:  http.StatusOK,
		data.CanonicalTargetsRole:   http.StatusNotFound,
		data.CanonicalRootRole:      http.StatusNotFound,
		"targets/delegation":        http.StatusNotFound,
		"somerandomrole":            http.StatusNotFound,
	}
	var buf bytes.Buffer
	for role, expectedStatus := range rolesToStatus {
		res, err := http.Post(
			fmt.Sprintf("%s/v2/gun/_trust/tuf/%s.key", ts.URL, role),
			"text/plain", &buf)
		require.NoError(t, err)
		require.Equal(t, expectedStatus, res.StatusCode)
	}
}

func TestValidationErrorFormat(t *testing.T) {
	metaStore := storage.NewMultiplexingStore(notaryStorage.NewMemStorage(), notaryStorage.NewMemStorage(), storage.NewSignerMemoryStore())
	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewTestingAccessController("testUser")
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), nil, nil, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := store.NewHTTPStore(
		fmt.Sprintf("%s/v2/quay.io/apostille/_trust/tuf/", server.URL),
		"",
		"json",
		"key",
		http.DefaultTransport,
	)
	require.NoError(t, err)

	repo, _, err := testutils.EmptyRepo("quay.io/apostille")
	require.NoError(t, err)
	r, tg, sn, ts, err := testutils.Sign(repo)
	require.NoError(t, err)
	rs, rt, _, _, err := testutils.Serialize(r, tg, sn, ts)
	require.NoError(t, err)

	// No snapshot is passed, and the server doesn't have the snapshot key,
	// so ErrBadHierarchy
	err = client.SetMulti(map[string][]byte{
		data.CanonicalRootRole:    rs,
		data.CanonicalTargetsRole: rt,
	})
	require.Error(t, err)
	require.IsType(t, validation.ErrBadHierarchy{}, err)
}

func TestSigningUserPushSignerUserPull(t *testing.T) {
	gun := "quay.io/signingUser/testRepo"
	trust := testUtils.TrustServiceMock(t)
	signerStore := notaryStorage.NewMemStorage()
	rootRepo := testUtils.AlternateRootRepoMock(t, trust, "quay-root")
	metaStore := storage.NewMultiplexingStore(signerStore, storage.NewAlternateRootMemStorage(trust, *rootRepo, signerStore), storage.NewSignerMemoryStore())
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewTestingAccessController("signingUser")
	handler := TrustMultiplexerHandler(ac, ctx, trust, nil, nil, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := store.NewHTTPStore(
		fmt.Sprintf("%s/v2/%s/_trust/tuf/", server.URL, gun),
		"",
		"json",
		"key",
		http.DefaultTransport,
	)
	require.NoError(t, err)

	repo := testUtils.AlternateRootRepoMock(t, trust, gun)
	require.NoError(t, err)
	r, tg, sn, ts, err := testutils.Sign(repo)
	require.NoError(t, err)
	rootJson, targetsJson, ssJson, tsJson, err := testutils.Serialize(r, tg, sn, ts)
	require.NoError(t, err)

	err = client.SetMulti(map[string][]byte{
		data.CanonicalRootRole:      rootJson,
		data.CanonicalTargetsRole:   targetsJson,
		data.CanonicalSnapshotRole:  ssJson,
		data.CanonicalTimestampRole: tsJson,
	})
	require.NoError(t, err)

	serverRootJson, err := client.GetSized(data.CanonicalRootRole, -1)
	require.NoError(t, err)
	require.Equal(t, rootJson, serverRootJson)

	serverTargetsJson, err := client.GetSized(data.CanonicalTargetsRole, -1)
	require.NoError(t, err)
	require.Equal(t, targetsJson, serverTargetsJson)

	serverSnapshotJson, err := client.GetSized(data.CanonicalSnapshotRole, -1)
	require.NoError(t, err)
	require.Equal(t, ssJson, serverSnapshotJson)

	_, err = client.GetSized(data.CanonicalTimestampRole, -1)
	require.NoError(t, err)
}

func TestSigningUserPushNonSignerPull(t *testing.T) {
	gun := "quay.io/signingUser/testRepo"
	trust := testUtils.TrustServiceMock(t)
	signerStore := notaryStorage.NewMemStorage()
	rootRepo := testUtils.AlternateRootRepoMock(t, trust, "quay-root")
	metaStore := storage.NewMultiplexingStore(signerStore, storage.NewAlternateRootMemStorage(trust, *rootRepo, signerStore), storage.NewSignerMemoryStore())
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewTestingAccessController("signingUser")
	handler := TrustMultiplexerHandler(ac, ctx, trust, nil, nil, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := store.NewHTTPStore(
		fmt.Sprintf("%s/v2/%s/_trust/tuf/", server.URL, gun),
		"",
		"json",
		"key",
		http.DefaultTransport,
	)
	require.NoError(t, err)

	repo := testUtils.AlternateRootRepoMock(t, trust, gun)
	require.NoError(t, err)
	r, tg, sn, ts, err := testutils.Sign(repo)
	require.NoError(t, err)
	rootJson, targetsJson, ssJson, tsJson, err := testutils.Serialize(r, tg, sn, ts)
	require.NoError(t, err)

	err = client.SetMulti(map[string][]byte{
		data.CanonicalRootRole:      rootJson,
		data.CanonicalTargetsRole:   targetsJson,
		data.CanonicalSnapshotRole:  ssJson,
		data.CanonicalTimestampRole: tsJson,
	})
	require.NoError(t, err)

	testAC, ok := ac.(*auth.TestingAccessController)
	require.True(t, ok)
	testAC.Username = "nonsigning-user"

	serverRootJson, err := client.GetSized(data.CanonicalRootRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, rootJson, serverRootJson)

	serverTargetsJson, err := client.GetSized(data.CanonicalTargetsRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, targetsJson, serverTargetsJson)

	serverSnapshotJson, err := client.GetSized(data.CanonicalSnapshotRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, ssJson, serverSnapshotJson)

	serverTargetsReleasesJson, err := client.GetSized("targets/releases", -1)
	require.NoError(t, err)
	require.Equal(t, targetsJson, serverTargetsReleasesJson)

	_, err = client.GetSized(data.CanonicalTimestampRole, -1)
	require.NoError(t, err)
}

func TestPullingWithWildCardGivesSameRootKey(t *testing.T) {
	gun := "quay.io/signingUser/testRepo"
	trust := testUtils.TrustServiceMock(t)
	signerStore := notaryStorage.NewMemStorage()
	rootRepo := testUtils.AlternateRootRepoMock(t, trust, "quay.io/*")
	metaStore := storage.NewMultiplexingStore(signerStore, storage.NewAlternateRootMemStorage(trust, *rootRepo, signerStore), storage.NewSignerMemoryStore())
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewTestingAccessController("signingUser")
	handler := TrustMultiplexerHandler(ac, ctx, trust, nil, nil, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := store.NewHTTPStore(
		fmt.Sprintf("%s/v2/%s/_trust/tuf/", server.URL, gun),
		"",
		"json",
		"key",
		http.DefaultTransport,
	)
	require.NoError(t, err)

	repo := testUtils.AlternateRootRepoMock(t, trust, gun)
	require.NoError(t, err)
	r, tg, sn, ts, err := testutils.Sign(repo)
	require.NoError(t, err)
	rootJson, targetsJson, ssJson, tsJson, err := testutils.Serialize(r, tg, sn, ts)
	require.NoError(t, err)

	err = client.SetMulti(map[string][]byte{
		data.CanonicalRootRole:      rootJson,
		data.CanonicalTargetsRole:   targetsJson,
		data.CanonicalSnapshotRole:  ssJson,
		data.CanonicalTimestampRole: tsJson,
	})
	require.NoError(t, err)

	testAC, ok := ac.(*auth.TestingAccessController)
	require.True(t, ok)
	testAC.Username = "nonsigning-user"

	serverRootJson, err := client.GetSized(data.CanonicalRootRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, rootJson, serverRootJson)

	rootRepoRootJson, err := rootRepo.Root.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, rootRepoRootJson, serverRootJson)

	serverTargetsJson, err := client.GetSized(data.CanonicalTargetsRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, targetsJson, serverTargetsJson)

	serverSnapshotJson, err := client.GetSized(data.CanonicalSnapshotRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, ssJson, serverSnapshotJson)

	serverTargetsReleasesJson, err := client.GetSized("targets/releases", -1)
	require.NoError(t, err)
	require.Equal(t, targetsJson, serverTargetsReleasesJson)

	_, err = client.GetSized(data.CanonicalTimestampRole, -1)
	require.NoError(t, err)
}

func TestSigningUserPushSignerPullNonSignerPull(t *testing.T) {
	gun := "quay.io/signingUser/testRepo"
	trust := testUtils.TrustServiceMock(t)
	signerStore := notaryStorage.NewMemStorage()
	rootRepo := testUtils.AlternateRootRepoMock(t, trust, "quay-root")
	metaStore := storage.NewMultiplexingStore(signerStore, storage.NewAlternateRootMemStorage(trust, *rootRepo, signerStore), storage.NewSignerMemoryStore())
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewTestingAccessController("signingUser")
	handler := TrustMultiplexerHandler(ac, ctx, trust, nil, nil, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := store.NewHTTPStore(
		fmt.Sprintf("%s/v2/%s/_trust/tuf/", server.URL, gun),
		"",
		"json",
		"key",
		http.DefaultTransport,
	)
	require.NoError(t, err)

	repo := testUtils.AlternateRootRepoMock(t, trust, gun)
	require.NoError(t, err)
	r, tg, sn, ts, err := testutils.Sign(repo)
	require.NoError(t, err)
	rootJson, targetsJson, ssJson, tsJson, err := testutils.Serialize(r, tg, sn, ts)
	require.NoError(t, err)

	err = client.SetMulti(map[string][]byte{
		data.CanonicalRootRole:      rootJson,
		data.CanonicalTargetsRole:   targetsJson,
		data.CanonicalSnapshotRole:  ssJson,
		data.CanonicalTimestampRole: tsJson,
	})
	require.NoError(t, err)

	serverRootJson, err := client.GetSized(data.CanonicalRootRole, -1)
	require.NoError(t, err)
	require.Equal(t, rootJson, serverRootJson)

	serverTargetsJson, err := client.GetSized(data.CanonicalTargetsRole, -1)
	require.NoError(t, err)
	require.Equal(t, targetsJson, serverTargetsJson)

	serverSnapshotJson, err := client.GetSized(data.CanonicalSnapshotRole, -1)
	require.NoError(t, err)
	require.Equal(t, ssJson, serverSnapshotJson)

	_, err = client.GetSized(data.CanonicalTimestampRole, -1)
	require.NoError(t, err)

	testAC, ok := ac.(*auth.TestingAccessController)
	require.True(t, ok)
	testAC.Username = "nonsigning-user"

	serverRootJson, err = client.GetSized(data.CanonicalRootRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, rootJson, serverRootJson)

	serverTargetsJson, err = client.GetSized(data.CanonicalTargetsRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, targetsJson, serverTargetsJson)

	serverSnapshotJson, err = client.GetSized(data.CanonicalSnapshotRole, -1)
	require.NoError(t, err)
	require.NotEqual(t, ssJson, serverSnapshotJson)

	serverTargetsReleasesJson, err := client.GetSized("targets/releases", -1)
	require.NoError(t, err)
	require.Equal(t, targetsJson, serverTargetsReleasesJson)

	_, err = client.GetSized(data.CanonicalTimestampRole, -1)
	require.NoError(t, err)
}