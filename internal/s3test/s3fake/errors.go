package s3fake

import (
	"encoding/xml"
	"net/http"
)

// apiError is an S3 error: HTTP status, S3 code and message. The XML body it produces is what AWS
// S3 sends (no x-minio-* headers: clients must map by <Code>, and HEAD answers carry no body).
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

var errTable = map[string]apiError{
	"AccessDenied":                         {403, "AccessDenied", "Access Denied."},
	"InvalidAccessKeyId":                   {403, "InvalidAccessKeyId", "The AWS access key ID you provided does not exist in our records."},
	"SignatureDoesNotMatch":                {403, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your key and signing method."},
	"RequestTimeTooSkewed":                 {403, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large."},
	"AuthorizationHeaderMalformed":         {400, "AuthorizationHeaderMalformed", "The authorization header is malformed."},
	"NoSuchBucket":                         {404, "NoSuchBucket", "The specified bucket does not exist"},
	"NoSuchKey":                            {404, "NoSuchKey", "The specified key does not exist."},
	"NoSuchVersion":                        {404, "NoSuchVersion", "The specified version does not exist."},
	"NoSuchLifecycleConfiguration":         {404, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist"},
	"ObjectLockConfigurationNotFoundError": {404, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket"},
	"PreconditionFailed":                   {412, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold"},
	"BucketAlreadyOwnedByYou":              {409, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it."},
	"BucketNotEmpty":                       {409, "BucketNotEmpty", "The bucket you tried to delete is not empty"},
	"InvalidBucketState":                   {409, "InvalidBucketState", "The request is not valid with the current state of the bucket."},
	"InvalidBucketName":                    {400, "InvalidBucketName", "The specified bucket is not valid."},
	"InvalidArgument":                      {400, "InvalidArgument", "Invalid argument."},
	"InvalidRequest":                       {400, "InvalidRequest", "Invalid request."},
	"InvalidRange":                         {416, "InvalidRange", "The requested range is not satisfiable"},
	"MalformedXML":                         {400, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema."},
	"BadDigest":                            {400, "BadDigest", "The Content-MD5 or checksum value you specified did not match what we received."},
	"XAmzContentSHA256Mismatch":            {400, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed."},
	"KeyTooLongError":                      {400, "KeyTooLongError", "Your key is too long"},
	"MissingContentLength":                 {411, "MissingContentLength", "You must provide the Content-Length HTTP header."},
	"IncompleteBody":                       {400, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header."},
	"EntityTooLarge":                       {400, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size."},
	"IllegalLocationConstraintException":   {400, "IllegalLocationConstraintException", "The unspecified location constraint is incompatible for the region specific endpoint this request was sent to."},
	"MethodNotAllowed":                     {405, "MethodNotAllowed", "The specified method is not allowed against this resource."},
	"NotImplemented":                       {501, "NotImplemented", "A header you provided implies functionality that is not implemented"},
	"SlowDown":                             {503, "SlowDown", "Please reduce your request rate."},
	"ServiceUnavailable":                   {503, "ServiceUnavailable", "Service is unable to handle request."},
	"InternalError":                        {500, "InternalError", "We encountered an internal error. Please try again."},
	"RequestTimeout":                       {400, "RequestTimeout", "Your socket connection to the server was not read from or written to within the timeout period."},
}

func apiErr(code string) *apiError {
	e, ok := errTable[code]
	if !ok {
		e = apiError{500, code, code}
	}
	return &e
}

func apiErrf(code, msg string) *apiError {
	e := apiErr(code)
	e.Message = msg
	return e
}

type xmlError struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	BucketName string   `xml:"BucketName,omitempty"`
	Key        string   `xml:"Key,omitempty"`
	Resource   string   `xml:"Resource,omitempty"`
	RequestID  string   `xml:"RequestId"`
	HostID     string   `xml:"HostId"`
}

// writeError sends the XML error body (none for HEAD, as on S3).
func writeError(w http.ResponseWriter, r *http.Request, rc *reqCtx, e *apiError) {
	h := w.Header()
	h.Set("Content-Type", "application/xml")
	h.Set("Server", serverName)
	h.Set("x-amz-request-id", rc.id)
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	body, _ := xml.Marshal(xmlError{Code: e.Code, Message: e.Message, BucketName: rc.bucket, Key: rc.key, Resource: r.URL.Path, RequestID: rc.id, HostID: "s3fake"})
	out := append([]byte(xml.Header), body...)
	h.Set("Content-Length", itoa(len(out)))
	w.WriteHeader(e.Status)
	_, _ = w.Write(out)
}
