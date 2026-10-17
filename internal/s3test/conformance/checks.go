package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// Checks returns the whole suite in running order.
func Checks() []Check {
	return []Check{
		{"bucket/create-head-delete", "", checkBucketLifecycle},
		{"bucket/missing-bucket-errors", "", checkMissingBucket},
		{"bucket/location", "", checkLocation},
		{"bucket/delete-nonempty-refused", "", checkDeleteNonEmpty},
		{"object/put-get-roundtrip-sizes", "", checkRoundtrip},
		{"object/metadata-and-content-type", "", checkMetadata},
		{"object/stat", "", checkStat},
		{"object/overwrite", "", checkOverwrite},
		{"object/get-missing-is-nosuchkey", "", checkGetMissing},
		{"object/range-reads", "", checkRange},
		{"object/delete-is-idempotent", "", checkDelete},
		{"object/special-character-keys", "", checkSpecialKeys},
		{"object/long-keys", "", checkLongKeys},
		{"put/if-none-match-refuses-existing", "", checkIfNoneMatch},
		{"put/if-none-match-is-atomic-under-concurrency", "", checkIfNoneMatchRace},
		{"put/if-none-match-distinct-keys-all-succeed", "", checkIfNoneMatchDistinct},
		{"put/torn-upload-leaves-no-object", "", checkTornUpload},
		{"list/empty-bucket", "", checkListEmpty},
		{"list/prefix", "", checkListPrefix},
		{"list/delimiter-common-prefixes", "", checkListDelimiter},
		{"list/pagination-with-continuation-tokens", "", checkListPagination},
		{"list/pagination-over-common-prefixes", "", checkListPaginationPrefixes},
		{"list/start-after", "", checkListStartAfter},
		{"list/over-1000-keys", "", checkListMany},
		{"list/special-characters", "", checkListSpecial},
		{"list/deleted-keys-disappear", "", checkListAfterDelete},
		{"delete/multi-object", FeatureMultiDelete, checkMultiDelete},
		{"copy/server-side", FeatureCopy, checkCopy},
		{"config/versioning", FeatureVersioning, checkVersioning},
		{"config/lifecycle", FeatureLifecycle, checkLifecycle},
		{"config/object-lock", FeatureObjectLock, checkObjectLock},
		{"auth/wrong-secret-refused", "", checkWrongSecret},
		{"auth/unknown-access-key-refused", "", checkUnknownKey},
		{"auth/anonymous-refused", "", checkAnonymous},
		{"auth/tampered-request-refused", "", checkTampered},
		{"errors/xml-error-body", "", checkErrorBody},
	}
}

func checkBucketLifecycle(c *C) {
	b := c.Bucket()
	ok, err := c.Client.BucketExists(c.Ctx, b)
	c.must(err, "BucketExists")
	if !ok {
		c.Fatalf("a bucket just created does not exist")
	}
	// creating it again: an error (BucketAlreadyOwnedByYou/Exists) or, on us-east-1 AWS, a no-op
	if err := c.Client.MakeBucket(c.Ctx, b, minio.MakeBucketOptions{}); err != nil {
		c.wantErr(err, "second MakeBucket", "BucketAlreadyOwnedByYou", "BucketAlreadyExists")
	}
	c.must(c.Client.RemoveBucket(c.Ctx, b), "RemoveBucket")
	ok, err = c.Client.BucketExists(c.Ctx, b)
	c.must(err, "BucketExists after delete")
	if ok {
		c.Fatalf("a deleted bucket still exists")
	}
}

func checkMissingBucket(c *C) {
	name := "ibconf-nosuch-" + fmt.Sprint(time.Now().UnixNano()%1_000_000)
	ok, err := c.Client.BucketExists(c.Ctx, name)
	c.must(err, "BucketExists of a missing bucket")
	if ok {
		c.Fatalf("a missing bucket exists")
	}
	_, err = c.Client.PutObject(c.Ctx, name, "k", strings.NewReader("x"), 1, minio.PutObjectOptions{DisableMultipart: true})
	c.wantErr(err, "put into a missing bucket", "NoSuchBucket")
	_, err = c.get(name, "k", minio.GetObjectOptions{})
	c.wantErr(err, "get from a missing bucket", "NoSuchBucket")
	for o := range c.Client.ListObjects(c.Ctx, name, minio.ListObjectsOptions{Recursive: true}) {
		c.wantErr(o.Err, "list a missing bucket", "NoSuchBucket")
		break
	}
}

func checkLocation(c *C) {
	b := c.Bucket()
	if _, err := c.Client.GetBucketLocation(c.Ctx, b); err != nil {
		c.Fatalf("GetBucketLocation: %v", err)
	}
}

func checkDeleteNonEmpty(c *C) {
	b := c.Bucket()
	c.put(b, "k", []byte("x"))
	c.wantErr(c.Client.RemoveBucket(c.Ctx, b), "delete of a non-empty bucket", "BucketNotEmpty")
}

func checkRoundtrip(c *C) {
	b := c.Bucket()
	for _, n := range []int{0, 1, 1000, 65536, 1<<20 + 1, 12 << 20} {
		d := c.data(n)
		key := fmt.Sprintf("size/%d", n)
		info := c.put(b, key, d)
		if info.Size != int64(n) {
			c.Errorf("%s: upload reports size %d", key, info.Size)
		}
		got := c.mustGet(b, key)
		if !bytes.Equal(got, d) {
			c.Fatalf("%s: read back %d bytes that differ from the %d written", key, len(got), n)
		}
		// a single-part ETag is the MD5 of the content
		if st, err := c.Client.StatObject(c.Ctx, b, key, minio.StatObjectOptions{}); err != nil {
			c.Fatalf("stat %s: %v", key, err)
		} else if st.Size != int64(n) || st.ETag != md5hex(d) {
			c.Errorf("%s: stat size %d etag %s, want %d %s", key, st.Size, st.ETag, n, md5hex(d))
		}
	}
}

func checkMetadata(c *C) {
	b := c.Bucket()
	c.put(b, "m", []byte("hello"), func(o *minio.PutObjectOptions) {
		o.ContentType = "text/x-test"
		o.UserMetadata = map[string]string{"Origin": "conformance", "Gen": "7"}
	})
	st, err := c.Client.StatObject(c.Ctx, b, "m", minio.StatObjectOptions{})
	c.must(err, "stat")
	if st.ContentType != "text/x-test" {
		c.Errorf("content type %q", st.ContentType)
	}
	if st.UserMetadata["Origin"] != "conformance" || st.UserMetadata["Gen"] != "7" {
		c.Errorf("user metadata %v", st.UserMetadata)
	}
	c.put(b, "nometa", []byte("x"))
	st, err = c.Client.StatObject(c.Ctx, b, "nometa", minio.StatObjectOptions{})
	c.must(err, "stat nometa")
	if len(st.UserMetadata) != 0 {
		c.Errorf("an object without metadata reports %v", st.UserMetadata)
	}
}

func checkStat(c *C) {
	b := c.Bucket()
	before := time.Now().Add(-10 * time.Minute)
	c.put(b, "s", c.data(100))
	st, err := c.Client.StatObject(c.Ctx, b, "s", minio.StatObjectOptions{})
	c.must(err, "stat")
	if st.Size != 100 || st.Key != "s" {
		c.Errorf("stat %+v", st)
	}
	if st.LastModified.Before(before) || st.LastModified.After(time.Now().Add(10*time.Minute)) {
		c.Errorf("last modified %v is not near now", st.LastModified)
	}
	_, err = c.Client.StatObject(c.Ctx, b, "absent", minio.StatObjectOptions{})
	c.wantErr(err, "stat of a missing key", "NoSuchKey")
	if status(err) != 404 {
		c.Errorf("stat of a missing key: HTTP %d", status(err))
	}
}

func checkOverwrite(c *C) {
	b := c.Bucket()
	c.put(b, "o", []byte("first"))
	c.put(b, "o", []byte("second-longer"))
	if got := string(c.mustGet(b, "o")); got != "second-longer" {
		c.Fatalf("overwrite: read %q", got)
	}
	keys, _ := c.list(b, minio.ListObjectsOptions{Recursive: true})
	c.equalStrings("keys after an overwrite", keys, []string{"o"})
}

func checkGetMissing(c *C) {
	b := c.Bucket()
	_, err := c.get(b, "absent", minio.GetObjectOptions{})
	c.wantErr(err, "get of a missing key", "NoSuchKey")
	if status(err) != 404 {
		c.Errorf("HTTP %d", status(err))
	}
}

func checkRange(c *C) {
	b := c.Bucket()
	d := c.data(1000)
	c.put(b, "r", d)
	cases := []struct {
		name       string
		start, end int64
		want       []byte
	}{
		{"first 10", 0, 9, d[:10]},
		{"middle", 100, 199, d[100:200]},
		{"open ended", 990, 0, d[990:]},
		{"suffix", 0, -5, d[995:]},
		{"to the end and beyond", 900, 5000, d[900:]},
		{"single byte", 7, 7, d[7:8]},
	}
	for _, tc := range cases {
		var o minio.GetObjectOptions
		c.must(o.SetRange(tc.start, tc.end), tc.name)
		got, err := c.get(b, "r", o)
		if err != nil {
			c.Errorf("range %s: %v", tc.name, err)
			continue
		}
		if !bytes.Equal(got, tc.want) {
			c.Errorf("range %s: got %d bytes (want %d) or wrong content", tc.name, len(got), len(tc.want))
		}
	}
	var o minio.GetObjectOptions
	c.must(o.SetRange(5000, 6000), "range")
	_, err := c.get(b, "r", o)
	c.wantErr(err, "range beyond the end", "InvalidRange")
}

func checkDelete(c *C) {
	b := c.Bucket()
	c.put(b, "d", []byte("x"))
	c.must(c.Client.RemoveObject(c.Ctx, b, "d", minio.RemoveObjectOptions{}), "delete")
	_, err := c.Client.StatObject(c.Ctx, b, "d", minio.StatObjectOptions{})
	c.wantErr(err, "stat after delete", "NoSuchKey")
	c.must(c.Client.RemoveObject(c.Ctx, b, "d", minio.RemoveObjectOptions{}), "second delete of the same key")
	c.must(c.Client.RemoveObject(c.Ctx, b, "never-existed", minio.RemoveObjectOptions{}), "delete of a key that never existed")
}

var specialKeys = []string{
	"space in name", "plus+sign", "percent%20literal", "amp&equals=q", "question?mark", "ünï/çödé/файл", "tilde~dot.", "a/b/c/d",
	"semi;colon,comma", "quote'dq\"", "hash#frag", "paren(1)[2]{3}", "star*at@dollar$", "100%",
}

func checkSpecialKeys(c *C) {
	b := c.Bucket()
	for i, k := range specialKeys {
		d := []byte(fmt.Sprintf("content-%d", i))
		c.put(b, k, d)
		if got := c.mustGet(b, k); !bytes.Equal(got, d) {
			c.Errorf("key %q: read back %q", k, got)
		}
		if _, err := c.Client.StatObject(c.Ctx, b, k, minio.StatObjectOptions{}); err != nil {
			c.Errorf("stat %q: %v", k, err)
		}
	}
	// a key that differs only by encoding must not alias another
	_, err := c.Client.StatObject(c.Ctx, b, "percent 20literal", minio.StatObjectOptions{})
	c.wantErr(err, "a similar key")
}

func checkLongKeys(c *C) {
	b := c.Bucket()
	seg := strings.Repeat("k", 200)
	key := strings.Join([]string{seg, seg, seg, seg, seg}, "/") // 1004 bytes
	c.put(b, key, []byte("long"))
	if got := string(c.mustGet(b, key)); got != "long" {
		c.Fatalf("long key read back %q", got)
	}
	keys, _ := c.list(b, minio.ListObjectsOptions{Recursive: true})
	c.equalStrings("long key listing", keys, []string{key})
}

func putIfAbsent(c *C, b, key string, data []byte) error {
	o := minio.PutObjectOptions{DisableMultipart: true}
	o.SetMatchETagExcept("*")
	_, err := c.Client.PutObject(c.Ctx, b, key, bytes.NewReader(data), int64(len(data)), o)
	return err
}

func checkIfNoneMatch(c *C) {
	b := c.Bucket()
	c.must(putIfAbsent(c, b, "once", []byte("first")), "first conditional put")
	err := putIfAbsent(c, b, "once", []byte("second"))
	c.wantErr(err, "second conditional put", "PreconditionFailed")
	if status(err) != 412 {
		c.Errorf("second conditional put: HTTP %d, want 412", status(err))
	}
	if got := string(c.mustGet(b, "once")); got != "first" {
		c.Fatalf("a refused conditional put changed the object to %q", got)
	}
	// an unconditional put still overwrites; after a delete the key is free again
	c.put(b, "once", []byte("forced"))
	c.must(c.Client.RemoveObject(c.Ctx, b, "once", minio.RemoveObjectOptions{}), "delete")
	c.must(putIfAbsent(c, b, "once", []byte("again")), "conditional put after delete")
	if got := string(c.mustGet(b, "once")); got != "again" {
		c.Fatalf("read %q", got)
	}
}

func checkIfNoneMatchRace(c *C) {
	b := c.Bucket()
	for round := 0; round < 4; round++ {
		key := fmt.Sprintf("race/%d", round)
		const n = 16
		var wins atomic.Int32
		winner := make([]atomic.Bool, n)
		var failed atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := putIfAbsent(c, b, key, []byte(fmt.Sprintf("writer-%02d-%s", i, strings.Repeat("x", 4000))))
				switch {
				case err == nil:
					wins.Add(1)
					winner[i].Store(true)
				case code(err) == "PreconditionFailed" || status(err) == 412:
					failed.Add(1)
				default:
					// S3 may answer 409 ConditionalRequestConflict to a loser that raced a writer in flight
					if code(err) == "ConditionalRequestConflict" || status(err) == 409 {
						failed.Add(1)
					} else {
						c.Errorf("racer %d: unexpected error %v", i, err)
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 {
			c.Fatalf("round %d: %d writers succeeded with If-None-Match: *, exactly 1 must", round, wins.Load())
		}
		got := string(c.mustGet(b, key))
		for i := 0; i < n; i++ {
			if winner[i].Load() && !strings.HasPrefix(got, fmt.Sprintf("writer-%02d-", i)) {
				c.Fatalf("round %d: writer %d won but the object holds %.20q", round, i, got)
			}
		}
	}
}

func checkIfNoneMatchDistinct(c *C) {
	b := c.Bucket()
	err := parallel(64, 16, func(i int) error {
		return putIfAbsent(c, b, fmt.Sprintf("distinct/%03d", i), []byte("v"))
	})
	c.must(err, "parallel conditional puts of distinct keys")
}

func checkTornUpload(c *C) {
	b := c.Bucket()
	full := c.data(3 << 20)
	o := minio.PutObjectOptions{DisableMultipart: true}
	o.SetMatchETagExcept("*")
	_, err := c.Client.PutObject(c.Ctx, b, "torn", &tearReader{r: bytes.NewReader(full), left: 200 << 10}, int64(len(full)), o)
	if err == nil {
		c.Fatalf("an upload whose body failed midway reported success")
	}
	// the service must never show a partial object (give a slow service a moment to settle)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		_, serr := c.Client.StatObject(c.Ctx, b, "torn", minio.StatObjectOptions{})
		if serr == nil {
			c.Fatalf("a torn upload left an object behind")
		}
		if code(serr) != "NoSuchKey" {
			c.Fatalf("stat after a torn upload: %v", serr)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// and the key is still free for a conditional create
	c.must(putIfAbsent(c, b, "torn", []byte("whole")), "conditional put after the torn one")
}

func checkListEmpty(c *C) {
	b := c.Bucket()
	keys, prefixes := c.list(b, minio.ListObjectsOptions{Recursive: true})
	if len(keys)+len(prefixes) != 0 {
		c.Fatalf("an empty bucket lists %v %v", keys, prefixes)
	}
	keys, prefixes = c.list(b, minio.ListObjectsOptions{Prefix: "a/"})
	if len(keys)+len(prefixes) != 0 {
		c.Fatalf("an empty bucket lists %v %v", keys, prefixes)
	}
}

func populate(c *C, b string, keys []string) {
	err := parallel(len(keys), 16, func(i int) error {
		_, err := c.Client.PutObject(c.Ctx, b, keys[i], strings.NewReader(keys[i]), int64(len(keys[i])), minio.PutObjectOptions{DisableMultipart: true})
		return err
	})
	c.must(err, "populate")
}

func checkListPrefix(c *C) {
	b := c.Bucket()
	all := []string{"a/1", "a/2", "a/sub/3", "ab/4", "abc", "b/5", "b/6", "top"}
	populate(c, b, all)
	cases := []struct {
		prefix string
		want   []string
	}{
		{"", sortedCopy(all)},
		{"a/", []string{"a/1", "a/2", "a/sub/3"}},
		{"a", []string{"a/1", "a/2", "a/sub/3", "ab/4", "abc"}},
		{"ab", []string{"ab/4", "abc"}},
		{"a/sub/", []string{"a/sub/3"}},
		{"zzz", nil},
		{"abc", []string{"abc"}},
		{"b/5", []string{"b/5"}},
	}
	for _, tc := range cases {
		keys, _ := c.list(b, minio.ListObjectsOptions{Prefix: tc.prefix, Recursive: true})
		c.equalStrings(fmt.Sprintf("recursive listing of prefix %q", tc.prefix), keys, tc.want)
	}
	// sizes and etags are reported
	for o := range c.Client.ListObjects(c.Ctx, b, minio.ListObjectsOptions{Prefix: "top", Recursive: true}) {
		c.must(o.Err, "list")
		if o.Size != 3 || o.ETag != md5hex([]byte("top")) || o.LastModified.IsZero() {
			c.Errorf("listing entry %+v", o)
		}
	}
}

func checkListDelimiter(c *C) {
	b := c.Bucket()
	populate(c, b, []string{"chunks/ab/cd/1", "chunks/ab/ce/2", "chunks/ac/00/3", "chunks/zz", "chunks/top", "meta.json", "other/x"})
	keys, pre := c.list(b, minio.ListObjectsOptions{})
	c.equalStrings("level 0 objects", keys, []string{"meta.json"})
	c.equalStrings("level 0 prefixes", pre, []string{"chunks/", "other/"})
	keys, pre = c.list(b, minio.ListObjectsOptions{Prefix: "chunks/"})
	c.equalStrings("chunks/ objects", keys, []string{"chunks/top", "chunks/zz"})
	c.equalStrings("chunks/ prefixes", pre, []string{"chunks/ab/", "chunks/ac/"})
	keys, pre = c.list(b, minio.ListObjectsOptions{Prefix: "chunks/ab/"})
	c.equalStrings("chunks/ab/ objects", keys, nil)
	c.equalStrings("chunks/ab/ prefixes", pre, []string{"chunks/ab/cd/", "chunks/ab/ce/"})
	// a prefix that is not a directory boundary
	keys, pre = c.list(b, minio.ListObjectsOptions{Prefix: "chunks/a"})
	c.equalStrings("chunks/a objects", keys, nil)
	c.equalStrings("chunks/a prefixes", pre, []string{"chunks/ab/", "chunks/ac/"})
}

// pageThrough follows continuation tokens with a small page size and returns every entry once.
func pageThrough(c *C, b, prefix, delim string, pageSize int) (keys, prefixes []string, pages int) {
	token := ""
	for {
		res, err := c.Core.ListObjectsV2(b, prefix, "", token, delim, pageSize)
		c.must(err, "ListObjectsV2")
		pages++
		if len(res.Contents)+len(res.CommonPrefixes) > pageSize {
			c.Fatalf("a page of %d entries exceeds max-keys %d", len(res.Contents)+len(res.CommonPrefixes), pageSize)
		}
		if res.IsTruncated && res.NextContinuationToken == "" {
			c.Fatalf("a truncated page carries no continuation token")
		}
		for _, o := range res.Contents {
			keys = append(keys, o.Key)
		}
		for _, p := range res.CommonPrefixes {
			prefixes = append(prefixes, p.Prefix)
		}
		if !res.IsTruncated {
			return
		}
		if len(res.Contents)+len(res.CommonPrefixes) == 0 {
			c.Fatalf("a truncated page is empty: the listing cannot make progress")
		}
		token = res.NextContinuationToken
		if pages > 10000 {
			c.Fatalf("listing does not terminate")
		}
	}
}

func checkListPagination(c *C) {
	b := c.Bucket()
	var want []string
	for i := 0; i < 53; i++ {
		want = append(want, fmt.Sprintf("p/%03d", i))
	}
	populate(c, b, want)
	for _, size := range []int{1, 2, 7, 25, 53, 54, 1000} {
		keys, _, pages := pageThrough(c, b, "p/", "", size)
		c.equalStrings(fmt.Sprintf("paged listing, page size %d", size), keys, want)
		if wantPages := (len(want) + size - 1) / size; pages != wantPages {
			c.Errorf("page size %d: %d pages, want %d", size, pages, wantPages)
		}
	}
	// max-keys through the client option too
	var got []string
	for o := range c.Client.ListObjects(c.Ctx, b, minio.ListObjectsOptions{Prefix: "p/", Recursive: true, MaxKeys: 5}) {
		c.must(o.Err, "list")
		got = append(got, o.Key)
	}
	c.equalStrings("listing with MaxKeys 5", got, want)
}

func checkListPaginationPrefixes(c *C) {
	b := c.Bucket()
	var all []string
	for i := 0; i < 6; i++ {
		for j := 0; j < 4; j++ {
			all = append(all, fmt.Sprintf("d/%02d/%d", i, j))
		}
	}
	all = append(all, "d/aaa", "d/zzz")
	populate(c, b, all)
	for _, size := range []int{1, 2, 3, 4} {
		keys, pre, _ := pageThrough(c, b, "d/", "/", size)
		c.equalStrings(fmt.Sprintf("objects with page size %d", size), keys, []string{"d/aaa", "d/zzz"})
		c.equalStrings(fmt.Sprintf("prefixes with page size %d", size), pre, []string{"d/00/", "d/01/", "d/02/", "d/03/", "d/04/", "d/05/"})
	}
}

func checkListStartAfter(c *C) {
	b := c.Bucket()
	populate(c, b, []string{"s/a", "s/b", "s/c", "s/d"})
	res, err := c.Core.ListObjectsV2(b, "s/", "s/b", "", "", 100)
	c.must(err, "list with start-after")
	var keys []string
	for _, o := range res.Contents {
		keys = append(keys, o.Key)
	}
	c.equalStrings("start-after s/b", keys, []string{"s/c", "s/d"})
}

func checkListMany(c *C) {
	b := c.Bucket()
	const n = 1105
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("many/%05d", i)
	}
	err := parallel(n, 32, func(i int) error {
		_, err := c.Client.PutObject(c.Ctx, b, want[i], strings.NewReader("x"), 1, minio.PutObjectOptions{DisableMultipart: true})
		return err
	})
	c.must(err, "populate")
	var got []string
	for o := range c.Client.ListObjects(c.Ctx, b, minio.ListObjectsOptions{Prefix: "many/", Recursive: true}) {
		c.must(o.Err, "list")
		got = append(got, o.Key)
	}
	c.equalStrings("recursive listing across the 1000-key page boundary", got, want)
	res, err := c.Core.ListObjectsV2(b, "many/", "", "", "", 1000)
	c.must(err, "first page")
	if !res.IsTruncated || len(res.Contents) != 1000 {
		c.Errorf("first page: truncated=%v with %d keys, want a truncated page of 1000", res.IsTruncated, len(res.Contents))
	}
}

func checkListSpecial(c *C) {
	b := c.Bucket()
	populate(c, b, specialKeys)
	keys, _ := c.list(b, minio.ListObjectsOptions{Recursive: true})
	c.equalStrings("special-character keys", keys, sortedCopy(specialKeys))
	// by prefix with a special character
	keys, _ = c.list(b, minio.ListObjectsOptions{Prefix: "space ", Recursive: true})
	c.equalStrings("prefix with a space", keys, []string{"space in name"})
	keys, _ = c.list(b, minio.ListObjectsOptions{Prefix: "plus+", Recursive: true})
	c.equalStrings("prefix with a plus", keys, []string{"plus+sign"})
	keys, _ = c.list(b, minio.ListObjectsOptions{Prefix: "ünï/", Recursive: true})
	c.equalStrings("prefix with non-ASCII", keys, []string{"ünï/çödé/файл"})
}

func checkListAfterDelete(c *C) {
	b := c.Bucket()
	populate(c, b, []string{"x/1", "x/2", "x/3"})
	c.must(c.Client.RemoveObject(c.Ctx, b, "x/2", minio.RemoveObjectOptions{}), "delete")
	keys, _ := c.list(b, minio.ListObjectsOptions{Recursive: true})
	c.equalStrings("after deleting x/2", keys, []string{"x/1", "x/3"})
	c.must(c.Client.RemoveObject(c.Ctx, b, "x/1", minio.RemoveObjectOptions{}), "delete")
	c.must(c.Client.RemoveObject(c.Ctx, b, "x/3", minio.RemoveObjectOptions{}), "delete")
	keys, pre := c.list(b, minio.ListObjectsOptions{})
	if len(keys)+len(pre) != 0 {
		c.Fatalf("an emptied directory still lists %v %v", keys, pre)
	}
}

func checkMultiDelete(c *C) {
	b := c.Bucket()
	var keys []string
	for i := 0; i < 1010; i++ { // more than one multi-delete request (limit 1000)
		keys = append(keys, fmt.Sprintf("md/%04d", i))
	}
	populate(c, b, keys)
	ch := make(chan minio.ObjectInfo)
	go func() {
		defer close(ch)
		for _, k := range keys {
			ch <- minio.ObjectInfo{Key: k}
		}
		ch <- minio.ObjectInfo{Key: "md/never-existed"} // deleting a missing key is not an error
	}()
	for r := range c.Client.RemoveObjects(c.Ctx, b, ch, minio.RemoveObjectsOptions{}) {
		if r.Err != nil {
			c.Errorf("multi-delete %s: %v", r.ObjectName, r.Err)
		}
	}
	left, _ := c.list(b, minio.ListObjectsOptions{Recursive: true})
	if len(left) != 0 {
		c.Fatalf("%d objects survived the multi-delete: %v", len(left), head(left))
	}
}

func checkCopy(c *C) {
	b := c.Bucket()
	d := c.data(5000)
	c.put(b, "src", d, func(o *minio.PutObjectOptions) {
		o.ContentType = "application/x-src"
		o.UserMetadata = map[string]string{"K": "v"}
	})
	_, err := c.Client.CopyObject(c.Ctx, minio.CopyDestOptions{Bucket: b, Object: "dst/copy"}, minio.CopySrcOptions{Bucket: b, Object: "src"})
	c.must(err, "copy")
	if got := c.mustGet(b, "dst/copy"); !bytes.Equal(got, d) {
		c.Fatalf("the copy differs from its source")
	}
	st, err := c.Client.StatObject(c.Ctx, b, "dst/copy", minio.StatObjectOptions{})
	c.must(err, "stat")
	if st.ContentType != "application/x-src" || st.UserMetadata["K"] != "v" {
		c.Errorf("copy lost metadata: %q %v", st.ContentType, st.UserMetadata)
	}
	_, err = c.Client.CopyObject(c.Ctx, minio.CopyDestOptions{Bucket: b, Object: "x"}, minio.CopySrcOptions{Bucket: b, Object: "no-such-source"})
	c.wantErr(err, "copy of a missing source", "NoSuchKey")
}

func checkVersioning(c *C) {
	b := c.Bucket()
	vc, err := c.Client.GetBucketVersioning(c.Ctx, b)
	c.must(err, "GetBucketVersioning of a plain bucket")
	if vc.Status != "" {
		c.Fatalf("a plain bucket reports versioning %q", vc.Status)
	}
	c.must(c.Client.EnableVersioning(c.Ctx, b), "EnableVersioning")
	vc, err = c.Client.GetBucketVersioning(c.Ctx, b)
	c.must(err, "GetBucketVersioning")
	if vc.Status != minio.Enabled {
		c.Fatalf("versioning status %q after enabling", vc.Status)
	}
	i1 := c.put(b, "v", []byte("one"))
	i2 := c.put(b, "v", []byte("two"))
	if i1.VersionID == "" || i2.VersionID == "" || i1.VersionID == i2.VersionID {
		c.Fatalf("version ids %q %q", i1.VersionID, i2.VersionID)
	}
	c.must(c.Client.RemoveObject(c.Ctx, b, "v", minio.RemoveObjectOptions{}), "delete")
	_, err = c.Client.StatObject(c.Ctx, b, "v", minio.StatObjectOptions{})
	c.wantErr(err, "stat of a key with a delete marker", "NoSuchKey")
	keys, _ := c.list(b, minio.ListObjectsOptions{Recursive: true})
	if len(keys) != 0 {
		c.Fatalf("a deleted (marker) key is listed: %v", keys)
	}
	var versions, markers int
	for o := range c.Client.ListObjects(c.Ctx, b, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		c.must(o.Err, "list versions")
		if o.IsDeleteMarker {
			markers++
		} else {
			versions++
		}
	}
	if versions != 2 || markers != 1 {
		c.Fatalf("version listing: %d versions %d markers, want 2 and 1", versions, markers)
	}
	d, err := c.get(b, "v", minio.GetObjectOptions{VersionID: i1.VersionID})
	c.must(err, "get of an old version")
	if string(d) != "one" {
		c.Fatalf("old version holds %q", d)
	}
}

func checkLifecycle(c *C) {
	b := c.Bucket()
	_, err := c.Client.GetBucketLifecycle(c.Ctx, b)
	c.wantErr(err, "lifecycle of a plain bucket", "NoSuchLifecycleConfiguration")
	lc := lifecycle.NewConfiguration()
	lc.Rules = []lifecycle.Rule{{ID: "cleanup", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: "logs/"}, Expiration: lifecycle.Expiration{Days: 30}}}
	c.must(c.Client.SetBucketLifecycle(c.Ctx, b, lc), "SetBucketLifecycle")
	got, err := c.Client.GetBucketLifecycle(c.Ctx, b)
	c.must(err, "GetBucketLifecycle")
	if len(got.Rules) != 1 || got.Rules[0].ID != "cleanup" || got.Rules[0].Expiration.Days != 30 {
		c.Fatalf("lifecycle read back as %+v", got.Rules)
	}
	pfx := got.Rules[0].Prefix
	if f := got.Rules[0].RuleFilter.Prefix; f != "" {
		pfx = f
	}
	if pfx != "logs/" {
		c.Errorf("lifecycle prefix %q", pfx)
	}
}

func checkObjectLock(c *C) {
	b := c.Bucket()
	_, _, _, _, err := c.Client.GetObjectLockConfig(c.Ctx, b)
	c.wantErr(err, "object lock of a plain bucket", "ObjectLockConfigurationNotFoundError")
	lb := b + "-lock"
	c.must(c.Client.MakeBucket(c.Ctx, lb, minio.MakeBucketOptions{ObjectLocking: true}), "make a bucket with object lock")
	c.mu.Lock()
	c.buckets = append(c.buckets, lb)
	c.mu.Unlock()
	mode, days, unit := minio.Governance, uint(7), minio.Days
	c.must(c.Client.SetObjectLockConfig(c.Ctx, lb, &mode, &days, &unit), "SetObjectLockConfig")
	enabled, m, d, u, err := c.Client.GetObjectLockConfig(c.Ctx, lb)
	c.must(err, "GetObjectLockConfig")
	if enabled != "Enabled" || m == nil || *m != minio.Governance || d == nil || *d != 7 || u == nil || *u != minio.Days {
		c.Fatalf("object lock read back as %q %v %v %v", enabled, m, d, u)
	}
}

func checkWrongSecret(c *C) {
	b := c.Bucket()
	c.put(b, "k", []byte("x"))
	cl, err := newClient(c.Target, c.Target.AccessKey, c.Target.SecretKey+"-wrong")
	c.must(err, "client")
	_, err = cl.StatObject(c.Ctx, b, "k", minio.StatObjectOptions{})
	c.wantErr(err, "stat with a wrong secret")
	if status(err) != 403 {
		c.Errorf("wrong secret: HTTP %d, want 403", status(err))
	}
	_, err = cl.PutObject(c.Ctx, b, "evil", strings.NewReader("x"), 1, minio.PutObjectOptions{DisableMultipart: true})
	c.wantErr(err, "put with a wrong secret")
	if status(err) != 403 {
		c.Errorf("put with a wrong secret: HTTP %d, want 403", status(err))
	}
	if _, err := c.Client.StatObject(c.Ctx, b, "evil", minio.StatObjectOptions{}); err == nil {
		c.Errorf("a refused put created an object")
	}
	// the right credential still works afterwards
	c.must(func() error { _, err := c.Client.StatObject(c.Ctx, b, "k", minio.StatObjectOptions{}); return err }(), "stat with the right credential")
}

func checkUnknownKey(c *C) {
	b := c.Bucket()
	cl, err := newClient(c.Target, "no-such-access-key", "whatever-secret-123")
	c.must(err, "client")
	_, err = cl.StatObject(c.Ctx, b, "k", minio.StatObjectOptions{})
	c.wantErr(err, "stat with an unknown access key")
	if status(err) != 403 {
		c.Errorf("unknown access key: HTTP %d, want 403", status(err))
	}
	ok, err := cl.BucketExists(c.Ctx, b)
	if err == nil && ok {
		c.Errorf("BucketExists with an unknown access key succeeded")
	}
}

func checkAnonymous(c *C) {
	b := c.Bucket()
	c.put(b, "k", []byte("secret content"))
	scheme := "http"
	if c.Target.secure() {
		scheme = "https"
	}
	req, _ := http.NewRequestWithContext(c.Ctx, http.MethodGet, scheme+"://"+c.Target.host()+"/"+b+"/k", nil)
	st, body := c.do(req)
	if st != 403 || bytes.Contains(body, []byte("secret content")) {
		c.Fatalf("an unsigned GET got HTTP %d: %.80s", st, body)
	}
	req, _ = http.NewRequestWithContext(c.Ctx, http.MethodPut, scheme+"://"+c.Target.host()+"/"+b+"/anon", strings.NewReader("x"))
	if st, _ := c.do(req); st != 403 {
		c.Fatalf("an unsigned PUT got HTTP %d", st)
	}
	if _, err := c.Client.StatObject(c.Ctx, b, "anon", minio.StatObjectOptions{}); err == nil {
		c.Fatalf("an unsigned PUT created an object")
	}
}

func checkTampered(c *C) {
	b := c.Bucket()
	c.put(b, "k1", []byte("one"))
	c.put(b, "k2", []byte("two"))
	// positive control: the untouched signed request is accepted
	req := c.rawSigned(http.MethodGet, "/"+b+"/k1", "")
	if st, body := c.do(req); st != 200 || string(body) != "one" {
		c.Fatalf("an untouched signed GET got HTTP %d %.60q", st, body)
	}
	// the path changed after signing
	req = c.rawSigned(http.MethodGet, "/"+b+"/k1", "")
	req.URL.Path = "/" + b + "/k2"
	if st, body := c.do(req); st != 403 || string(body) == "two" {
		c.Fatalf("a GET whose path was changed after signing got HTTP %d", st)
	}
	// a query parameter added after signing
	req = c.rawSigned(http.MethodGet, "/"+b, "list-type=2&prefix=k1")
	req.URL.RawQuery = "list-type=2&prefix=k2"
	if st, _ := c.do(req); st != 403 {
		c.Fatalf("a listing whose query was changed after signing got HTTP %d", st)
	}
	// the method changed after signing
	req = c.rawSigned(http.MethodGet, "/"+b+"/k1", "")
	req.Method = http.MethodDelete
	if st, _ := c.do(req); st != 403 {
		c.Fatalf("a request whose method was changed after signing got HTTP %d", st)
	}
	if got := string(c.mustGet(b, "k1")); got != "one" {
		c.Fatalf("k1 changed to %q", got)
	}
}

func checkErrorBody(c *C) {
	b := c.Bucket()
	req := c.rawSigned(http.MethodGet, "/"+b+"/absent", "")
	resp, err := http.DefaultClient.Do(req)
	c.must(err, "raw GET")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 {
		c.Errorf("HTTP %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<Code>NoSuchKey</Code>") {
		c.Errorf("error body is not an S3 NoSuchKey document: %.200s", body)
	}
	req = c.rawSigned(http.MethodGet, "/ibconf-no-such-bucket-zzz/k", "")
	if st, body := c.do(req); st != 404 || !strings.Contains(string(body), "<Code>NoSuchBucket</Code>") {
		c.Errorf("missing bucket: HTTP %d %.200s", st, body)
	}
}

var (
	_ = context.Background
	_ = sort.Strings
)
