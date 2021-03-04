// Copyright 2018 Google LLC. All Rights Reserved.
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

package ctutil

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	ct "github.com/ctylim/certificate-transparency-go-p192"
	"github.com/ctylim/certificate-transparency-go-p192/client"
	"github.com/ctylim/certificate-transparency-go-p192/dnsclient"
	"github.com/ctylim/certificate-transparency-go-p192/jsonclient"
	"github.com/ctylim/certificate-transparency-go-p192/loglist"
	"github.com/ctylim/certificate-transparency-go-p192/x509"
	"github.com/google/trillian/merkle"
	"github.com/google/trillian/merkle/rfc6962"
)

// LogInfo holds the objects needed to perform per-log verification and
// validation of SCTs.
type LogInfo struct {
	Description string
	Client      client.CheckLogClient
	MMD         time.Duration
	Verifier    *ct.SignatureVerifier
	PublicKey   []byte

	mu      sync.RWMutex
	lastSTH *ct.SignedTreeHead
}

// NewLogInfo builds a LogInfo object based on a log list entry.
func NewLogInfo(log *loglist.Log, hc *http.Client) (*LogInfo, error) {
	url := log.URL
	if !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	lc, err := client.New(url, hc, jsonclient.Options{PublicKeyDER: log.Key, UserAgent: "ct-go-logclient"})
	if err != nil {
		return nil, fmt.Errorf("failed to create client for log %q: %v", log.Description, err)
	}
	return newLogInfo(log, lc)
}

// NewLogInfoOverDNSWrapper builds a LogInfo object that accesses logs via DNS, based on a log list entry.
// The inert http.Client argument allows this variant to be used interchangeably with NewLogInfo.
func NewLogInfoOverDNSWrapper(log *loglist.Log, _ *http.Client) (*LogInfo, error) {
	return NewLogInfoOverDNS(log)
}

// NewLogInfoOverDNS builds a LogInfo object that accesses logs via DNS, based on a log list entry.
func NewLogInfoOverDNS(log *loglist.Log) (*LogInfo, error) {
	if log.DNSAPIEndpoint == "" {
		return nil, fmt.Errorf("no available DNS endpoint for log %q", log.Description)
	}
	dc, err := dnsclient.New(log.DNSAPIEndpoint, jsonclient.Options{PublicKeyDER: log.Key})
	if err != nil {
		return nil, fmt.Errorf("failed to create DNS client for log %q: %v", log.Description, err)
	}
	return newLogInfo(log, dc)
}

func newLogInfo(log *loglist.Log, lc client.CheckLogClient) (*LogInfo, error) {
	logKey, err := x509.ParsePKIXPublicKey(log.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse public key data for log %q: %v", log.Description, err)
	}
	verifier, err := ct.NewSignatureVerifier(logKey)
	if err != nil {
		return nil, fmt.Errorf("failed to build verifier log %q: %v", log.Description, err)
	}
	mmd := time.Duration(log.MaximumMergeDelay) * time.Second
	return &LogInfo{
		Description: log.Description,
		Client:      lc,
		MMD:         mmd,
		Verifier:    verifier,
		PublicKey:   log.Key,
	}, nil
}

// LogInfoByHash holds LogInfo objects index by the SHA-256 hash of the log's public key.
type LogInfoByHash map[[sha256.Size]byte]*LogInfo

// LogInfoByKeyHash builds a map of LogInfo objects indexed by their key hashes.
func LogInfoByKeyHash(ll *loglist.LogList, hc *http.Client) (LogInfoByHash, error) {
	return logInfoByKeyHash(ll, hc, NewLogInfo)
}

// LogInfoByKeyHashOverDNS builds a map of LogInfo objects (for access over DNS) indexed by their key hashes.
func LogInfoByKeyHashOverDNS(ll *loglist.LogList, hc *http.Client) (LogInfoByHash, error) {
	return logInfoByKeyHash(ll, hc, NewLogInfoOverDNSWrapper)
}

func logInfoByKeyHash(ll *loglist.LogList, hc *http.Client, infoFactory func(*loglist.Log, *http.Client) (*LogInfo, error)) (map[[sha256.Size]byte]*LogInfo, error) {
	result := make(map[[sha256.Size]byte]*LogInfo)
	for _, log := range ll.Logs {
		h := sha256.Sum256(log.Key)
		li, err := infoFactory(&log, hc)
		if err != nil {
			return nil, err
		}
		result[h] = li
	}
	return result, nil
}

// LastSTH returns the last STH known for the log.
func (li *LogInfo) LastSTH() *ct.SignedTreeHead {
	li.mu.RLock()
	defer li.mu.RUnlock()
	return li.lastSTH
}

// SetSTH sets the last STH known for the log.
func (li *LogInfo) SetSTH(sth *ct.SignedTreeHead) {
	li.mu.Lock()
	defer li.mu.Unlock()
	li.lastSTH = sth
}

// VerifySCTSignature checks the signature in the SCT matches the given leaf (adjusted for the
// timestamp in the SCT) and log.
func (li *LogInfo) VerifySCTSignature(sct ct.SignedCertificateTimestamp, leaf ct.MerkleTreeLeaf) error {
	leaf.TimestampedEntry.Timestamp = sct.Timestamp
	if err := li.Verifier.VerifySCTSignature(sct, ct.LogEntry{Leaf: leaf}); err != nil {
		return fmt.Errorf("failed to verify SCT signature from log %q: %v", li.Description, err)
	}
	return nil
}

// VerifyInclusionLatest checks that the given Merkle tree leaf, adjusted for the provided timestamp,
// is present in the latest known tree size of the log.  If no tree size for the log is known, it will
// be queried.  On success, returns the index of the leaf in the log.
func (li *LogInfo) VerifyInclusionLatest(ctx context.Context, leaf ct.MerkleTreeLeaf, timestamp uint64) (int64, error) {
	sth := li.LastSTH()
	if sth == nil {
		var err error
		sth, err = li.Client.GetSTH(ctx)
		if err != nil {
			return -1, fmt.Errorf("failed to get current STH for %q log: %v", li.Description, err)
		}
		li.SetSTH(sth)
	}
	return li.VerifyInclusionAt(ctx, leaf, timestamp, sth.TreeSize, sth.SHA256RootHash[:])
}

// VerifyInclusion checks that the given Merkle tree leaf, adjusted for the provided timestamp,
// is present in the current tree size of the log.  On success, returns the index of the leaf
// in the log.
func (li *LogInfo) VerifyInclusion(ctx context.Context, leaf ct.MerkleTreeLeaf, timestamp uint64) (int64, error) {
	sth, err := li.Client.GetSTH(ctx)
	if err != nil {
		return -1, fmt.Errorf("failed to get current STH for %q log: %v", li.Description, err)
	}
	li.SetSTH(sth)
	return li.VerifyInclusionAt(ctx, leaf, timestamp, sth.TreeSize, sth.SHA256RootHash[:])
}

// VerifyInclusionAt checks that the given Merkle tree leaf, adjusted for the provided timestamp,
// is present in the given tree size & root hash of the log. On success, returns the index of the
// leaf in the log.
func (li *LogInfo) VerifyInclusionAt(ctx context.Context, leaf ct.MerkleTreeLeaf, timestamp, treeSize uint64, rootHash []byte) (int64, error) {
	leaf.TimestampedEntry.Timestamp = timestamp
	leafHash, err := ct.LeafHashForLeaf(&leaf)
	if err != nil {
		return -1, fmt.Errorf("failed to create leaf hash: %v", err)
	}

	rsp, err := li.Client.GetProofByHash(ctx, leafHash[:], treeSize)
	if err != nil {
		return -1, fmt.Errorf("failed to GetProofByHash(sct,size=%d): %v", treeSize, err)
	}

	verifier := merkle.NewLogVerifier(rfc6962.DefaultHasher)
	if err := verifier.VerifyInclusionProof(rsp.LeafIndex, int64(treeSize), rsp.AuditPath, rootHash, leafHash[:]); err != nil {
		return -1, fmt.Errorf("failed to verify inclusion proof at size %d: %v", treeSize, err)
	}
	return rsp.LeafIndex, nil
}
