package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coreos-inc/apostille/auth"
	"github.com/coreos-inc/apostille/servertest"
	"github.com/coreos-inc/apostille/storage"
	"github.com/coreos-inc/apostille/storagetest"
	registryAuth "github.com/docker/distribution/registry/auth"
	_ "github.com/docker/distribution/registry/auth/silly"
	"github.com/docker/notary"
	notaryServer "github.com/docker/notary/server"
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
	"time"
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

func TestRepoPrefixMatches(t *testing.T) {
	var gun data.GUN = "docker.io/notary"
	meta, cs, err := testutils.NewRepoMetadata(gun)
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, notaryStorage.NewMemStorage())
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	snChecksumBytes := sha256.Sum256(meta[data.CanonicalSnapshotRole])

	// successful gets
	handler := notaryServer.RootHandler(ctx, nil, cs, nil, nil, []string{"docker.io"})
	ts := httptest.NewServer(handler)

	url := fmt.Sprintf("%s/v2/%s/_trust/tuf/", ts.URL, gun)
	uploader, err := store.NewHTTPStore(url, "", "json", "key", http.DefaultTransport)
	require.NoError(t, err)

	// uploading is cool
	require.NoError(t, uploader.SetMulti(data.MetadataRoleMapToStringMap(meta)))
	// getting is cool
	_, err = uploader.GetSized(data.CanonicalSnapshotRole.String(), notary.MaxDownloadSize)
	require.NoError(t, err)

	_, err = uploader.GetSized(
		tufutils.ConsistentName(data.CanonicalSnapshotRole.String(), snChecksumBytes[:]), notary.MaxDownloadSize)
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
	var gun data.GUN = "docker.io/notary"
	meta, cs, err := testutils.NewRepoMetadata(gun)
	require.NoError(t, err)
	s := notaryStorage.NewMemStorage()

	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, s)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	snChecksumBytes := sha256.Sum256(meta[data.CanonicalSnapshotRole])

	// successful gets
	handler := notaryServer.RootHandler(ctx, nil, cs, nil, nil, []string{"nope"})
	ts := httptest.NewServer(handler)

	url := fmt.Sprintf("%s/v2/%s/_trust/tuf/", ts.URL, gun)
	uploader, err := store.NewHTTPStore(url, "", "json", "key", http.DefaultTransport)
	require.NoError(t, err)

	require.Error(t, uploader.SetMulti(data.MetadataRoleMapToStringMap(meta)))

	// update the storage so we don't fail just because the metadata is missing
	for _, roleName := range data.BaseRoles {
		require.NoError(t, s.UpdateCurrent(gun, notaryStorage.MetaUpdate{
			Role:     roleName,
			Data:     meta[roleName],
			Version:  1,
			Channels: []*notaryStorage.Channel{&storage.SignerRoot},
		}))
	}

	_, err = uploader.GetSized(data.CanonicalSnapshotRole.String(), notary.MaxDownloadSize)
	require.Error(t, err)

	_, err = uploader.GetSized(
		tufutils.ConsistentName(data.CanonicalSnapshotRole.String(), snChecksumBytes[:]), notary.MaxDownloadSize)
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
	ac := auth.NewConstantAccessController("testUser")

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
	trust := servertest.TrustServiceMock(t)
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewConstantAccessController("testUser")

	// successful gets
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), nil, nil, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	rolesToStatus := map[data.RoleName]int{
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
	trust := servertest.TrustServiceMock(t)
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)
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
		Role:     data.CanonicalTimestampRole,
		Version:  1,
		Data:     j,
		Channels: []*notaryStorage.Channel{&storage.SignerRoot},
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
		Role:     data.CanonicalTimestampRole,
		Version:  2,
		Data:     newTS,
		Channels: []*notaryStorage.Channel{&storage.SignerRoot},
	}})

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)

	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewConstantAccessController("signer")
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
	trust := servertest.TrustServiceMock(t)
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)

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
		Role:     data.CanonicalTimestampRole,
		Version:  1,
		Data:     j,
		Channels: []*notaryStorage.Channel{&storage.SignerRoot},
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
		Role:     data.CanonicalTimestampRole,
		Version:  2,
		Data:     newTS,
		Channels: []*notaryStorage.Channel{&storage.SignerRoot},
	}})

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)

	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewConstantAccessController("signer")
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
	trust := servertest.TrustServiceMock(t)
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)

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
	ac := auth.NewConstantAccessController("signer")
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
	require.Equal(t, "no-cache", r.Header.Get("Pragma"))
}

// RotateKey supports only timestamp and snapshot key rotation
func TestRotateKeyEndpoint(t *testing.T) {
	trust := servertest.TrustServiceMock(t)
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ccc := utils.NewCacheControlConfig(10, false)
	ac := auth.NewConstantAccessController("testUser")
	handler := TrustMultiplexerHandler(ac, ctx, signed.NewEd25519(), ccc, ccc, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	rolesToStatus := map[data.RoleName]int{
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
	trust := servertest.TrustServiceMock(t)
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)

	ctx := context.WithValue(
		context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	ac := auth.NewConstantAccessController("testUser")
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
		data.CanonicalRootRole.String():    rs,
		data.CanonicalTargetsRole.String(): rt,
	})
	require.Error(t, err)
	require.IsType(t, validation.ErrBadHierarchy{}, err)
}

func TestSigningUserPushNonSignerPullSignerPull(t *testing.T) {
	trust := servertest.TrustServiceMock(t)
	ac := auth.NewConstantAccessController("signer")
	gun := data.GUN("quay.io/signingUser/testRepo")
	server, client := testServerAndClient(t, gun, trust, ac)
	defer server.Close()
	repo := servertest.CreateRepo(t, gun, trust)
	meta := servertest.PushRepo(t, repo, client)

	servertest.RemoteEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])

	ac.TUFRoot = "quay"

	servertest.RemoteNotEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])
	servertest.RemoteEqual(t, client, "targets/releases", meta[data.CanonicalTargetsRole])
}


func TestSigningUserPushTwiceNonSignerPullSignerPull(t *testing.T) {
	trust := servertest.TrustServiceMock(t)
	ac := auth.NewConstantAccessController("signer")
	gun := data.GUN("quay.io/signingUser/testRepo")
	server, client := testServerAndClient(t, gun, trust, ac)
	defer server.Close()
	repo := servertest.CreateRepo(t, gun, trust)
	meta := servertest.PushRepo(t, repo, client)

	servertest.RemoteEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])

	// push just snapshot
	ss, err := repo.SignSnapshot(time.Now().AddDate(1,1,1))
	require.NoError(t, err)
	snapshot, err := json.Marshal(ss)
	require.NoError(t,err)

	stringMeta := make(map[string][]byte, 1)
	stringMeta[data.CanonicalSnapshotRole.String()] = snapshot
	err = client.SetMulti(stringMeta)
	require.NoError(t, err)

	servertest.RemoteEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteEqual(t, client, data.CanonicalSnapshotRole, snapshot)

	// switch to quay root
	ac.TUFRoot = "quay"
	servertest.RemoteNotEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])
	servertest.RemoteEqual(t, client, "targets/releases", meta[data.CanonicalTargetsRole])
}

func TestSigningUserPushSignerPullNonSignerPull(t *testing.T) {
	trust := servertest.TrustServiceMock(t)
	ac := auth.NewConstantAccessController("signer")
	gun := data.GUN("quay.io/signingUser/testRepo")
	server, client := testServerAndClient(t, gun, trust, ac)
	defer server.Close()
	repo := servertest.CreateRepo(t, gun, trust)
	meta := servertest.PushRepo(t, repo, client)

	ac.TUFRoot = "quay"

	servertest.RemoteNotEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])
	servertest.RemoteEqual(t, client, "targets/releases", meta[data.CanonicalTargetsRole])

	ac.TUFRoot = "signer"

	servertest.RemoteEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])
}

func TestSigningUserPushWithDelegations(t *testing.T) {
	trust := servertest.TrustServiceMock(t)
	ac := auth.NewConstantAccessController("signer")
	gun := data.GUN("quay.io/signingUser/testRepo")
	server, client := testServerAndClient(t, gun, trust, ac)
	defer server.Close()
	repo := servertest.CreateRepo(t, gun, trust)
	repo = servertest.AddDelegationToRepo(t, gun, trust, repo, "targets/ci")
	meta := servertest.PushRepo(t, repo, client)

	ac.TUFRoot = "quay"

	servertest.RemoteNotEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteNotEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])
	servertest.RemoteEqual(t, client, "targets/releases", meta[data.CanonicalTargetsRole])
	servertest.RemoteEqual(t, client, "targets/ci", meta[data.RoleName("targets/ci")])

	ac.TUFRoot = "signer"

	servertest.RemoteEqual(t, client, data.CanonicalRootRole, meta[data.CanonicalRootRole])
	servertest.RemoteEqual(t, client, data.CanonicalTargetsRole, meta[data.CanonicalTargetsRole])
	servertest.RemoteEqual(t, client, data.CanonicalSnapshotRole, meta[data.CanonicalSnapshotRole])
	servertest.RemoteEqual(t, client, "targets/ci", meta[data.RoleName("targets/ci")])
}

func testServerAndClient(t *testing.T, gun data.GUN, trust signed.CryptoService, ac registryAuth.AccessController) (*httptest.Server, store.RemoteStore) {
	metaStore := storagetest.MultiplexingMetaStoreMock(t, trust)
	ctx := context.WithValue(context.Background(), notary.CtxKeyMetaStore, metaStore)
	ctx = context.WithValue(ctx, notary.CtxKeyKeyAlgo, data.ED25519Key)

	handler := TrustMultiplexerHandler(ac, ctx, trust, nil, nil, nil)
	server := httptest.NewServer(handler)

	client, err := store.NewHTTPStore(
		fmt.Sprintf("%s/v2/%s/_trust/tuf/", server.URL, gun),
		"",
		"json",
		"key",
		http.DefaultTransport,
	)
	require.NoError(t, err)
	return server, client
}
