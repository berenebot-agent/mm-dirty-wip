package apispec

// This file maps each operation to its request body, primary success body and
// query parameters. It is separate from the ordered route table in spec.go so
// the documentation order stays readable and the wire contract is reviewable in
// one place. Routes() applies it; a test fails if a mapping targets an unknown
// operation or names a schema that does not exist.

// routeIO is the wire contract for one operation.
type routeIO struct {
	Request            string
	RequestContentType string
	Response           string
	Query              []Param
}

// query parameter shorthands.
func p(name, typ string, required bool, desc string) Param {
	return Param{Name: name, Type: typ, Required: required, Description: desc}
}

var (
	qLimit    = p("limit", "integer", false, "Maximum items to return; the store clamps to the advertised page-size ceiling.")
	qBefore   = p("before", "string", false, "Opaque keyset cursor; return items older than this.")
	qInbox    = p("inbox", "string", false, "Scope results to one inbox id.")
	qLabel    = p("label", "array", false, "Repeatable; a message must carry every listed label.")
	qFrom     = p("from", "string", false, "Match the RFC5322 From address.")
	qTo       = p("to", "string", false, "Match a recipient address.")
	qUnread   = p("unread", "boolean", false, "Filter by read state.")
	qHasAtt   = p("has_attachment", "boolean", false, "Only messages with attachments.")
	qSpam     = p("spam", "boolean", false, "List only messages classified as spam.")
	qInclSpam = p("include_spam", "boolean", false, "Include spam alongside ordinary mail.")
	qAfter    = p("after", "string", false, "Durable event cursor (evt_...); return events after it.")
	qTimeout  = p("timeout", "integer", false, "Seconds to wait; the server caps this value.")
	qWait     = p("wait", "boolean", false, "Block until delivery completes instead of returning as soon as queued.")
	qActive   = p("active", "boolean", false, "Only outstanding send requests.")
	qAddress  = p("address", "string", false, "openagent.email compatibility alias for inbox.")
	qQ        = p("q", "string", true, "FTS5 query string.")
)

var routeIOByKey = map[string]routeIO{
	// Discovery.
	"GET /v1/bootstrap": {Response: "Bootstrap"},
	"GET /v1/limits":    {Response: "Limits"},

	// Inboxes.
	"GET /v1/inboxes":         {Response: "InboxList"},
	"POST /v1/inboxes":        {Request: "InboxCreate", Response: "Inbox"},
	"GET /v1/inboxes/{id}":    {Response: "Inbox"},
	"PATCH /v1/inboxes/{id}":  {Request: "InboxPatch", Response: "Inbox"},
	"DELETE /v1/inboxes/{id}": {},

	// Identities (openagent.email compatibility).
	"GET /v1/identities":              {Response: "IdentityList"},
	"POST /v1/identities":             {Request: "IdentityCreate", Response: "IdentityCreated"},
	"DELETE /v1/identities/{address}": {Response: "Deleted"},

	// Messages.
	"GET /v1/messages": {Response: "MessageList", Query: []Param{
		qInbox, p("thread", "string", false, "Scope to one thread id."), qFrom, qTo, qUnread, qHasAtt, qLabel, qSpam, qInclSpam, qBefore, qLimit, qAddress,
	}},
	"GET /v1/messages/wait": {Response: "Message", Query: []Param{
		qInbox, qAfter, qTimeout, qInclSpam,
	}},
	"POST /v1/messages/wait": {Request: "MessageWaitBody", Response: "Message", Query: []Param{
		qInbox, qAfter, qTimeout, qInclSpam,
	}},
	"GET /v1/messages/{id}":       {Response: "Message"},
	"PATCH /v1/messages/{id}":     {Request: "MessagePatch", Response: "Message"},
	"DELETE /v1/messages/{id}":    {},
	"POST /v1/messages/{id}/seen": {Request: "SeenBody", Response: "SeenResult"},

	// Attachments.
	"GET /v1/messages/{id}/attachments": {Response: "AttachmentList"},
	"GET /v1/attachments/{id}":          {},

	// Threads.
	"GET /v1/threads":               {Response: "ThreadList", Query: []Param{qInbox, qLimit}},
	"GET /v1/threads/{id}":          {Response: "ThreadDetail"},
	"GET /v1/threads/{id}/messages": {Response: "MessageList"},

	// Search.
	"GET /v1/search": {Response: "MessageList", Query: []Param{
		qQ, qInbox, qLabel, qFrom, qTo, qHasAtt, qBefore, qLimit,
	}},

	// Labels.
	"GET /v1/labels": {Response: "LabelList"},

	// Events.
	"GET /v1/events":        {Response: "EventList", Query: []Param{qAfter, qInbox, qLimit}},
	"GET /v1/events/wait":   {Response: "EventList", Query: []Param{qAfter, qTimeout, qInbox}},
	"GET /v1/events/stream": {},

	// Send and reply.
	"POST /v1/send":                {Request: "SendBody", Response: "SendQueued", Query: []Param{qWait}},
	"POST /v1/messages/{id}/reply": {Request: "ReplyBody", Response: "ReplyResult", Query: []Param{qWait}},

	// Drafts.
	"GET /v1/drafts":                             {Response: "DraftList", Query: []Param{qInbox, qBefore, qLimit}},
	"POST /v1/drafts":                            {Request: "DraftWrite", Response: "DraftWriteResult"},
	"GET /v1/drafts/{id}":                        {Response: "Draft"},
	"PATCH /v1/drafts/{id}":                      {Request: "DraftWrite", Response: "DraftWriteResult"},
	"DELETE /v1/drafts/{id}":                     {},
	"POST /v1/drafts/{id}/send":                  {Request: "DraftWrite", Response: "SendQueued"},
	"GET /v1/drafts/{id}/attachments":            {Response: "DraftAttachmentList"},
	"POST /v1/drafts/{id}/attachments":           {Request: "DraftAttachmentUpload", RequestContentType: "multipart/form-data", Response: "DraftAttachmentList"},
	"GET /v1/drafts/{id}/attachments/{attId}":    {},
	"DELETE /v1/drafts/{id}/attachments/{attId}": {},

	// Send requests.
	"POST /v1/drafts/{id}/request-send":        {Request: "RequestSendBody", Response: "Draft"},
	"POST /v1/drafts/{id}/cancel-send-request": {Response: "Draft"},
	"POST /v1/drafts/{id}/approve":             {Request: "FeedbackBody", Response: "ApproveResult"},
	"POST /v1/drafts/{id}/reject":              {Request: "FeedbackBody", Response: "Draft"},
	"GET /v1/drafts/{id}/send-request":         {Response: "SendRequest"},
	"GET /v1/send-requests":                    {Response: "SendRequestList", Query: []Param{qInbox, qActive, qLimit}},

	// Outbox.
	"GET /v1/outbox":             {Response: "MessageList", Query: []Param{qInbox, qLimit}},
	"POST /v1/outbox/{id}/retry": {},
	"DELETE /v1/outbox/{id}":     {},

	// Admin: domains.
	"GET /v1/admin/domains":                           {Response: "DomainList"},
	"POST /v1/admin/domains":                          {Request: "DomainCreate", Response: "Domain"},
	"PATCH /v1/admin/domains/{id}":                    {Request: "DomainPatch", Response: "Updated"},
	"DELETE /v1/admin/domains/{id}":                   {},
	"GET /v1/admin/domains/{id}/sending":              {Response: "DomainConfig"},
	"PUT /v1/admin/domains/{id}/sending":              {Request: "DomainSendingPut", Response: "DomainConfig"},
	"DELETE /v1/admin/domains/{id}/sending":           {},
	"GET /v1/admin/domains/{id}/receiving":            {Response: "DomainConfig"},
	"PUT /v1/admin/domains/{id}/receiving":            {Request: "DomainReceivingPut", Response: "DomainConfig"},
	"DELETE /v1/admin/domains/{id}/receiving":         {},
	"GET /v1/admin/domains/{id}/sending/deliveries":   {Response: "DeliveryAttemptList", Query: []Param{qLimit, qBefore}},
	"GET /v1/admin/domains/{id}/receiving/deliveries": {Response: "DomainLogList", Query: []Param{qLimit, qBefore}},

	// Admin: keys.
	"GET /v1/admin/keys":         {Response: "APIKeyList"},
	"POST /v1/admin/keys":        {Request: "KeyCreateBody", Response: "KeyCreated"},
	"DELETE /v1/admin/keys/{id}": {},

	// Admin: external aliases.
	"GET /v1/admin/inboxes/{id}/external-aliases":                              {Response: "ExternalAliasList"},
	"POST /v1/admin/inboxes/{id}/external-aliases":                             {Request: "ExternalAliasCreate", Response: "ExternalAlias"},
	"PATCH /v1/admin/inboxes/{id}/external-aliases/{aliasID}":                  {Request: "ExternalAliasPatch", Response: "ExternalAlias"},
	"DELETE /v1/admin/inboxes/{id}/external-aliases/{aliasID}":                 {},
	"GET /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending":            {Response: "ExternalAliasSending"},
	"PUT /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending":            {Request: "ExternalAliasSendingPut", Response: "ExternalAlias"},
	"DELETE /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending":         {},
	"GET /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending/deliveries": {Response: "DeliveryAttemptList", Query: []Param{qLimit, qBefore}},

	// Admin: Hermes.
	"GET /v1/admin/hermes":         {Response: "HermesConnectionList"},
	"POST /v1/admin/hermes/enroll": {Request: "HermesEnrollBody", Response: "HermesEnrollment"},
	"PUT /v1/admin/hermes/{id}":    {Request: "HermesUpdateBody", Response: "HermesUpdateResult"},
	"DELETE /v1/admin/hermes/{id}": {},

	// Admin: clients.
	"GET /v1/admin/clients":                        {Response: "ClientList"},
	"GET /v1/admin/clients/webhooks":               {Response: "WebhookClientList"},
	"POST /v1/admin/clients/webhooks":              {Request: "WebhookCreateBody", Response: "WebhookCreated"},
	"PUT /v1/admin/clients/webhooks/{id}":          {Request: "WebhookUpdateBody", Response: "Updated"},
	"DELETE /v1/admin/clients/webhooks/{id}":       {},
	"POST /v1/admin/clients/webhooks/{id}/rotate":  {Response: "WebhookRotateResult"},
	"POST /v1/admin/clients/webhooks/{id}/enabled": {Request: "WebhookEnabledBody", Response: "WebhookEnabledResult"},
}

// ContractKeys returns the "METHOD /path" keys that carry wire-contract
// metadata. A drift test asserts every key names a real operation, so a typo in
// routeIOByKey cannot silently drop a request or response schema.
func ContractKeys() []string {
	out := make([]string, 0, len(routeIOByKey))
	for k := range routeIOByKey {
		out = append(out, k)
	}
	return out
}

// decorate returns a copy of routes with the wire contract from routeIOByKey
// applied.
func decorate(routes []Route) []Route {
	out := make([]Route, len(routes))
	copy(out, routes)
	for i := range out {
		io, ok := routeIOByKey[out[i].Method+" "+out[i].Path]
		if !ok {
			continue
		}
		out[i].Request = io.Request
		out[i].RequestContentType = io.RequestContentType
		out[i].Response = io.Response
		out[i].Query = io.Query
	}
	return out
}
