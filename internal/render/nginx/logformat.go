package nginx

// Access log contract between the nginx renderer (engine slice) and the log
// ingester (observe slice). Every http server block must `set $relay_host_id
// <host id>;` (empty for the default server); every stream server block must
// `set $relay_stream_id <stream id>;`. $relay_tunnel is defined by nginx.conf.

const (
	AccessLogFile       = "access.log"        // under Env.LogDir
	StreamAccessLogFile = "stream-access.log" // under Env.LogDir
	ErrorLogFile        = "error.log"         // under Env.LogDir
)

// HTTPLogFormat is emitted in the http {} block.
const HTTPLogFormat = `log_format relay_json escape=json '{'
  '"ts":"$msec",'
  '"host_id":"$relay_host_id",'
  '"host":"$host",'
  '"method":"$request_method",'
  '"uri":"$request_uri",'
  '"protocol":"$server_protocol",'
  '"scheme":"$scheme",'
  '"status":"$status",'
  '"bytes_sent":"$body_bytes_sent",'
  '"request_length":"$request_length",'
  '"request_time":"$request_time",'
  '"upstream_addr":"$upstream_addr",'
  '"upstream_status":"$upstream_status",'
  '"upstream_connect_time":"$upstream_connect_time",'
  '"upstream_header_time":"$upstream_header_time",'
  '"upstream_response_time":"$upstream_response_time",'
  '"remote_addr":"$remote_addr",'
  '"user_agent":"$http_user_agent",'
  '"referer":"$http_referer",'
  '"accept":"$http_accept",'
  '"x_forwarded_for":"$http_x_forwarded_for",'
  '"request_id":"$request_id",'
  '"ssl_protocol":"$ssl_protocol",'
  '"remote_user":"$remote_user",'
  '"tunnel":"$relay_tunnel"'
'}';`

// StreamLogFormat is emitted in the stream {} block.
const StreamLogFormat = `log_format relay_stream_json escape=json '{'
  '"ts":"$msec",'
  '"stream_id":"$relay_stream_id",'
  '"protocol":"$protocol",'
  '"remote_addr":"$remote_addr",'
  '"server_port":"$server_port",'
  '"upstream_addr":"$upstream_addr",'
  '"status":"$status",'
  '"bytes_sent":"$bytes_sent",'
  '"bytes_received":"$bytes_received",'
  '"session_time":"$session_time",'
  '"upstream_connect_time":"$upstream_connect_time",'
  '"tunnel":"$relay_tunnel"'
'}';`

// AccessLogRecord mirrors HTTPLogFormat. All values are strings as emitted;
// numeric fields may be "-" or comma-separated lists (multiple upstreams).
type AccessLogRecord struct {
	TS                   string `json:"ts"`
	HostID               string `json:"host_id"`
	Host                 string `json:"host"`
	Method               string `json:"method"`
	URI                  string `json:"uri"`
	Protocol             string `json:"protocol"`
	Scheme               string `json:"scheme"`
	Status               string `json:"status"`
	BytesSent            string `json:"bytes_sent"`
	RequestLength        string `json:"request_length"`
	RequestTime          string `json:"request_time"`
	UpstreamAddr         string `json:"upstream_addr"`
	UpstreamStatus       string `json:"upstream_status"`
	UpstreamConnectTime  string `json:"upstream_connect_time"`
	UpstreamHeaderTime   string `json:"upstream_header_time"`
	UpstreamResponseTime string `json:"upstream_response_time"`
	RemoteAddr           string `json:"remote_addr"`
	UserAgent            string `json:"user_agent"`
	Referer              string `json:"referer"`
	Accept               string `json:"accept"`
	XForwardedFor        string `json:"x_forwarded_for"`
	RequestID            string `json:"request_id"`
	SSLProtocol          string `json:"ssl_protocol"`
	RemoteUser           string `json:"remote_user"`
	Tunnel               string `json:"tunnel"` // tunnel gateway id, "" for direct connections
}

type StreamLogRecord struct {
	TS                  string `json:"ts"`
	StreamID            string `json:"stream_id"`
	Protocol            string `json:"protocol"`
	RemoteAddr          string `json:"remote_addr"`
	ServerPort          string `json:"server_port"`
	UpstreamAddr        string `json:"upstream_addr"`
	Status              string `json:"status"`
	BytesSent           string `json:"bytes_sent"`
	BytesReceived       string `json:"bytes_received"`
	SessionTime         string `json:"session_time"`
	UpstreamConnectTime string `json:"upstream_connect_time"`
	Tunnel              string `json:"tunnel"` // tunnel gateway id, "" for direct connections
}
