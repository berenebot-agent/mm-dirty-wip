package httpapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/auth"
	"github.com/dellarb/mammothmail/internal/htmlsanitize"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/mxwire"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/timezone"
	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

const pageTemplate = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">{{if noindex}}<meta name="robots" content="noindex,nofollow">{{end}}<title>{{.Title}} · Mammoth Mail</title><link rel="icon" href="/favicon.ico" sizes="any"><link rel="icon" href="/favicon-16x16.png" type="image/png" sizes="16x16"><link rel="icon" href="/favicon-32x32.png" type="image/png" sizes="32x32"><link rel="apple-touch-icon" href="/apple-touch-icon.png"><style>
.mx-editor{border-top:1px solid #ddd;margin-top:12px;padding-top:12px}.mx-editor summary{cursor:pointer;font-weight:600;padding:8px 0}.mx-editor details{border-top:1px solid #eee;margin:8px 0}.mx-editor .row{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}.domain-dialog .mx-editor .dialog-actions{position:static;border-top:0}.mx-setup[hidden],[data-mx-mode][hidden]{display:none}.mx-setup code{overflow-wrap:anywhere}@media(max-width:540px){.mx-editor .row{grid-template-columns:1fr}}
.setup-url-row{display:flex;align-items:center;gap:8px;margin:8px 0;min-width:0}.setup-url-row pre{flex:1;min-width:0;margin:0;padding:8px;background:#f2f3f5;border-radius:6px;white-space:pre-wrap;overflow-wrap:anywhere}.setup-url-row .icon-btn{flex:0 0 auto;width:32px;height:32px;padding:6px}.setup-url-row .copied{background:#e6f4ea;color:#137333}
[hidden]{display:none!important}html{height:100%}body{font:15px system-ui,sans-serif;max-width:none;margin:0;padding:24px 32px;color:#202124;background:#fafafa;min-height:100dvh;box-sizing:border-box;display:flex;flex-direction:column}a{color:#1557b0}header{display:flex;flex-direction:row;align-items:center;gap:16px;flex-wrap:wrap;margin-bottom:16px}.brandrow{display:flex;align-items:center;gap:8px}.menu{display:flex;align-items:center;gap:16px;flex-wrap:wrap;flex:1;min-width:0}.menu-left,.menu-right{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.menu-right{margin-left:auto}.menu form{margin:0}.quota{font-size:13px;color:#666;white-space:nowrap;padding:0 4px}h1,h2,h3{margin:.4em 0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,300px),1fr));gap:16px}.dashboard-grid{grid-template-columns:repeat(2,minmax(0,1fr))}.dashboard-full-card{grid-column:1/-1}@media (max-width:720px){.dashboard-grid{grid-template-columns:1fr}}.search-form{display:flex;align-items:center;gap:8px;margin:4px 0 12px}.search-form input{flex:1;min-width:0;margin:0}.search-form button{flex:0 0 auto}.provider-picker{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:4px 0 12px}.provider-picker select{margin:0;flex:1;min-width:180px;max-width:320px}.cfg-form .dialog-actions{margin-top:12px}.cfg-summary{display:grid;grid-template-columns:minmax(90px,max-content) 1fr;gap:2px 12px;margin:8px 0;font-size:13px}.cfg-summary dt{color:#666}.cfg-summary dd{margin:0;overflow-wrap:anywhere;min-width:0}.actions-left{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin:8px 0}.actions-left form{margin:0}.wrap{overflow-wrap:anywhere;word-break:break-all}.table-wrap{overflow-x:auto}.card{background:white;border:1px solid #ddd;border-radius:10px;padding:16px;margin-bottom:16px;display:flex;flex-direction:column;min-width:0}.card[hidden]{display:none}.muted{color:#666}input,select,textarea{font:inherit;padding:8px;border:1px solid #bbb;border-radius:6px;box-sizing:border-box}input,select,textarea{width:100%;margin:4px 0 10px}.btn,button{display:inline-block;font:inherit;padding:8px 14px;border:1px solid #111;border-radius:6px;background:#111;color:#fff;text-decoration:none;line-height:1.2;cursor:pointer;box-sizing:border-box}.btn:hover,button:hover{background:#000;border-color:#000}.secondary{background:#fff;color:#111;border-color:#bbb}.btn.secondary:hover,button.secondary:hover{background:#f2f3f5;border-color:#999}.danger{color:#b00020;border-color:#e0a0aa}.btn.danger:hover,button.danger:hover{background:#fdecef;border-color:#c66}.approve{color:#137333;border-color:#9ad0ae}.btn.approve:hover,button.approve:hover{background:#e6f4ea;border-color:#137333}.actions{display:flex;gap:8px;align-items:center;justify-content:flex-end;flex-wrap:wrap}.actions form{margin:0}.btn-sm{height:30px;padding:0 10px;font-size:13px;display:inline-flex;align-items:center;justify-content:center}.row .btn-narrow{padding:8px 7px;flex:0 0 auto}.sub{font-size:12px;color:#666;margin-top:2px}.row{display:flex;gap:8px;align-items:center}.row .email-field{margin:0;flex:1}.row>*{flex:1}.slist{list-style:none;margin:0 0 12px;padding:0;border:1px solid #ddd;border-radius:8px;overflow:hidden}.slist li{display:flex;align-items:center;gap:8px;padding:8px 10px;border-bottom:1px solid #eee}.slist li:last-child{border-bottom:0}.slist .addr{flex:1;font-size:14px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist .empty{color:#666;font-size:13px;padding:12px}.slist .icon-btn{flex:0 0 auto}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:8px;border-bottom:1px solid #eee;vertical-align:top}table.dense th,table.dense td{padding:4px 8px;line-height:1.3;vertical-align:middle}table.dense .icon-btn{height:26px;width:26px;padding:0}table.dense .unread-pill,table.dense .pending-pill{padding:2px 10px}.one-import-table{font-size:13px}.one-import-table th,.one-import-table td{vertical-align:middle}.one-import-table td:first-child{width:56px}.one-import-table th:last-child,.one-import-table td:last-child{width:168px}.one-import-table select{width:auto;min-width:150px;margin:0;padding:5px 6px}.one-import-table tbody tr{cursor:pointer}.one-import-table tbody tr:hover td{background:#f6f9ff}.one-import-table tbody tr.selected td{background:#eef4fd}.one-import-table tbody tr.selected td:first-child{box-shadow:inset 3px 0 0 #1557b0}code,pre{background:#f3f3f3;padding:2px 4px;border-radius:4px}pre{padding:12px;white-space:pre-wrap;overflow:auto}.secret{border:1px solid #d5b400;background:#fffbe6;padding:12px;border-radius:8px;word-break:break-all}.secret pre{background:transparent;padding:0;margin:8px 0 0;white-space:pre-wrap;word-break:break-all}.copy-note{font-size:13px;color:#8a6d00;margin-top:8px}.msgbody{white-space:pre-wrap}.pill{display:inline-block;background:#eee;border-radius:999px;padding:2px 7px;font-size:12px}.connector-chips{display:flex;align-items:center;gap:5px;flex-wrap:wrap}.connector-chip{height:28px;width:28px;padding:0;border:0;background:transparent;font-size:12px;gap:0}.connector-chip svg,.connector-chip img{width:18px;height:18px;display:block;object-fit:contain}button.connector-chip:hover,button.connector-chip:focus-visible{background:transparent;border-color:transparent}.connector-add{height:28px;min-width:28px;padding:0 8px}.connector-view-hidden{display:none!important}.connector-list{display:flex;flex-direction:column;gap:8px;margin-top:10px}.access-list{display:flex;flex-direction:column;gap:8px;margin:8px 0 14px}.access-row{display:flex;align-items:center;gap:10px;border:1px solid #e3e3e3;border-radius:8px;padding:9px 10px;background:#fff}.access-row .access-main{flex:1;min-width:0}.access-row .access-name{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.access-row .access-meta{font-size:12px;color:#5f6368;margin-top:2px}.access-row .access-role{flex:0 0 auto}.access-domain-send{flex:0 0 auto}.access-domain-send button{border-left:0;min-width:104px}.access-domain-send-spacer{width:106px;flex:0 0 106px}.access-row .access-remove{flex:0 0 auto}.access-empty{color:#666;font-size:13px;padding:8px 2px}
.connector-row{display:flex;align-items:center;gap:8px;border:1px solid #e3e3e3;border-radius:8px;padding:9px 10px;background:#fff}.connector-type-icon{width:26px;height:26px;display:inline-flex;align-items:center;justify-content:center;flex:0 0 26px;color:#5f6368}.connector-type-icon svg,.connector-type-icon img{width:20px;height:20px;display:block;object-fit:contain}.connector-brand-openclaw{filter:grayscale(1) brightness(0)}.connector-row-main{flex:1;min-width:0;text-align:left}.connector-row-name{font-weight:600}.connector-row-meta{font-size:12px;color:#5f6368;margin-top:2px}.connector-editor{margin-top:0}.connector-editor .dialog-actions{position:static;border-top:0;padding:0;margin-top:12px}.connector-editor [data-inline-bearer] .row{align-items:center;margin:4px 0 10px}.connector-editor [data-inline-bearer] .row input{margin:0}.subdomain-tag{display:inline-flex;align-items:center;vertical-align:middle;color:#8a9096}.subdomain-tag svg{width:14px;height:14px;display:block}.log-table{font-size:12px}.log-detail{font-size:10px;word-break:break-all}.error{background:#fee;border:1px solid #e99;padding:10px}.ok{background:#efe;border:1px solid #9c9;padding:10px}dialog{border:0;border-radius:10px;padding:20px;max-width:480px;width:92%;box-sizing:border-box}dialog.inbox-settings[open]{border:0;border-radius:12px;padding:0;width:min(1080px,96vw);height:min(90dvh,880px);max-width:96vw;max-height:96dvh;display:flex;flex-direction:column;overflow:hidden}.inbox-settings-head{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:14px 20px;border-bottom:1px solid #e5e5e5;flex:0 0 auto}.inbox-settings-head h2{margin:0;font-size:1.25em}.inbox-settings-body{display:grid;grid-template-columns:200px minmax(0,1fr);flex:1 1 auto;min-height:0}.inbox-settings-nav{display:flex;flex-direction:column;gap:2px;padding:12px;border-right:1px solid #e5e5e5;overflow-y:auto}.inbox-settings-nav .dialog-tab{text-align:left;border:0;border-radius:8px;padding:9px 12px;font-size:14px;background:transparent}.inbox-settings-nav .dialog-tab.active{background:#e8eaed}.inbox-settings-panels{position:relative;overflow-y:auto;padding:20px;min-height:0;overscroll-behavior:contain}.inbox-settings-footer{display:flex;align-items:center;gap:8px;padding:12px 20px;border-top:1px solid #e5e5e5;background:#fff;flex:0 0 auto}.inbox-settings-footer .dialog-danger{margin-right:auto}.inbox-settings-footer .spacer{margin-left:auto}.inbox-subview{display:none}.inbox-settings--subview .inbox-settings-body{grid-template-columns:1fr}.inbox-settings--subview .inbox-settings-nav{display:none}.inbox-settings--chooser .inbox-settings-body{grid-template-columns:minmax(0,1fr)}.inbox-settings--chooser .inbox-settings-nav{display:none}.inbox-settings--subview .inbox-settings-footer [type=submit]{display:none}.inbox-settings--subview .inbox-settings-panels>*{display:none}.inbox-settings--subview .inbox-settings-panels>.inbox-subview.active{display:block}.inbox-subview-head{display:flex;align-items:center;gap:10px;margin:0 0 12px}.inbox-subview-head h3{margin:0}.alias-editor{border:1px solid #ddd;border-radius:8px;padding:12px;margin:0 0 12px;background:#fafafa}.alias-editor[hidden]{display:none}@media(max-width:680px){.inbox-settings-body{display:flex;flex-direction:column}.inbox-settings-nav{flex-direction:row;flex-wrap:nowrap;overflow-x:auto;border-right:0;border-bottom:1px solid #e5e5e5;padding:8px}.inbox-settings-nav .dialog-tab{white-space:nowrap;flex:0 0 auto}}.add-inbox-choose{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin:12px 0 4px}.add-inbox-choice{display:flex;flex-direction:column;align-items:flex-start;gap:6px;min-height:170px;padding:22px 20px;border:1px solid #bbb;border-radius:12px;background:#fff;color:#202124;text-align:left;cursor:pointer}.add-inbox-choice:hover,.add-inbox-choice:focus-visible{background:#f2f3f5;border-color:#999}.add-inbox-choice:focus-visible{outline:2px solid #1557b0;outline-offset:2px}.add-inbox-choice .choice-icon{width:34px;height:34px;color:#202124;flex:0 0 auto;margin-bottom:6px}.add-inbox-choice .choice-title{font-size:16px;font-weight:700;color:#202124}.add-inbox-choice .choice-desc{font-size:13px;line-height:1.45;color:#5f6368;font-weight:400}
.inbox-settings--subview:has(.inbox-subview.active[data-inbox-subview=connectors]) .inbox-settings-footer [type=submit]{display:inline-block}.inbox-settings--subview:has(.inbox-subview.active[data-inbox-subview=connectors]) #inbox-edit-delete{display:none}
.provider-choose{display:grid;gap:10px;margin:6px 0 4px}
dialog.inbox-settings .email-field,dialog.inbox-settings .inbox-schedule-grid,dialog.inbox-settings .remote-fields,dialog.inbox-settings .provider-box{max-width:var(--setup-form-width)}.provider-choice{display:flex;align-items:center;gap:14px;width:100%;padding:14px 16px;border:1px solid #bbb;border-radius:12px;background:#fff;color:#202124;text-align:left;cursor:pointer}
.provider-choice:hover,.provider-choice:focus-visible{background:#f2f3f5;border-color:#999}.provider-choice-advanced{border-color:#111}.provider-choice-advanced:hover,.provider-choice-advanced:focus-visible{border-color:#111}
.provider-choice:focus-visible{outline:2px solid #1557b0;outline-offset:2px}
.provider-choice .provider-logo{width:32px;height:32px;flex:0 0 32px;display:inline-flex;align-items:center;justify-content:center;color:#202124}
.provider-choice .provider-logo svg{width:100%;height:100%;display:block}
.provider-choice .provider-text{flex:1;min-width:0;display:flex;flex-direction:column;gap:2px}
.provider-choice .provider-title{font-size:15px;font-weight:700;color:#202124}
.provider-choice .provider-desc{font-size:13px;line-height:1.4;color:#5f6368;font-weight:400}
.provider-choice .provider-chevron{width:18px;height:18px;flex:0 0 18px;color:#80868b}
[data-standalone-wizard]{max-width:900px;margin-left:auto;margin-right:auto}
.wizard-step-title{display:flex;align-items:center;gap:10px;margin:0 0 4px;font-size:1.5em}
.wizard-step-title .wizard-logo{width:26px;height:26px;flex:0 0 26px;display:inline-flex;align-items:center;justify-content:center}
.wizard-step-title .wizard-logo svg{width:100%;height:100%;display:block}
/* A left-aligned Back control at the top of each wizard step, so the way back
sits in the same place on every step and returns to the screen the user came
from. */
.wizard-step-nav{display:flex;align-items:center;margin:0 0 10px}
.wizard-step-nav .wizard-back{display:inline-flex;align-items:center;gap:6px;height:32px;padding:0 12px}
.wizard-step-nav .wizard-back:before{content:"\2190";font-size:15px;line-height:1}
.wizard-box{border:1px solid #ddd;border-radius:10px;padding:16px;margin:14px 0;background:#fff}
.wizard-box>.section-head:first-child,.wizard-box>h3:first-child{margin-top:0}
.wizard-box .dialog-actions{margin-top:14px}
[data-google-status]:empty,[data-microsoft-status]:empty{display:none}
.ms-connect-grid{display:grid;gap:10px;margin:6px 0 2px}
@media(min-width:560px){.ms-connect-grid{grid-template-columns:1fr 1fr}}
.ms-connect{display:flex;align-items:center;gap:12px;width:100%;padding:12px 14px;border:1px solid #8c8c8c;border-radius:4px;background:#fff;color:#5e5e5e;text-align:left;font-weight:600}
.ms-connect:hover,.ms-connect:focus-visible{background:#f3f3f3;border-color:#5e5e5e;color:#5e5e5e}
.ms-connect:focus-visible{outline:2px solid #1557b0;outline-offset:2px}
.ms-connect .ms-logo{width:20px;height:20px;flex:0 0 20px}
.ms-connect .ms-logo svg{width:100%;height:100%;display:block}
.ms-connect .ms-text{display:flex;flex-direction:column;gap:1px;min-width:0}
.ms-connect .ms-text b{font-size:14px;color:#2f2f2f}
.ms-connect .ms-text span{font-size:12px;font-weight:400;color:#5f6368;white-space:normal}
.paste-hint{margin:0 0 8px;padding:10px 12px;border:1px solid #1557b0;border-radius:8px;background:#e8f0fe;color:#1557b0;font-weight:700;text-align:center}
.wizard-box>.paste-hint+h3{margin-top:0}
.paste-hint.flash{animation:paste-hint-flash 1s ease-in-out 4}
@keyframes paste-hint-flash{0%,100%{background:#e8f0fe;color:#1557b0;box-shadow:0 0 0 0 rgba(21,87,176,0)}50%{background:#fff3b0;color:#202124;box-shadow:0 0 0 6px rgba(21,87,176,.15)}}
.notice.warn{background:#fff8e1;border:1px solid #e6c34a;padding:10px}
@media (prefers-reduced-motion:reduce){.paste-hint.flash{animation:none;background:#fff3b0;color:#202124}}.remote-fields{border:1px solid #ddd;border-radius:8px;margin:14px 0 4px;padding:0 12px;background:#fafafa}.remote-fields[hidden]{display:none}.remote-fields>summary{cursor:pointer;font-weight:600;padding:10px 0;list-style:revert}.remote-fields-body{padding:2px 0 10px}.remote-fields-body .section-head:first-child{margin-top:0}.inbox-schedule{margin:14px 0 4px}.inbox-schedule[hidden]{display:none}.inbox-schedule-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px}.inbox-schedule-grid label{margin-top:0}.inbox-schedule-grid input{margin-top:6px}@media(max-width:560px){.inbox-schedule-grid{grid-template-columns:1fr}}@media(max-width:560px){.add-inbox-choose{grid-template-columns:1fr}}#key-dialog{width:820px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}#key-dialog.key-dialog--wide{width:820px;max-width:calc(100vw - 24px)}#key-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0}dialog::backdrop{background:rgba(0,0,0,.45)}.toolbar{display:flex;gap:12px;align-items:center;margin-bottom:12px}.toolbar a{text-decoration:none}.msghead{display:grid;grid-template-columns:minmax(0,1fr) max-content;align-items:start;gap:12px}.msghead h1{margin-top:0;min-width:0;overflow-wrap:anywhere}.msghead .actions{grid-column:2;justify-self:end;flex-wrap:nowrap}.msghead .actions .icon-btn{height:36px;width:36px;min-width:36px;padding:0;flex:0 0 auto;box-sizing:border-box}.inboxhead{display:flex;align-items:center;gap:12px;flex-wrap:wrap}.inboxtitle{margin:0;font-size:1.9em;display:flex;align-items:center;gap:10px;flex-wrap:wrap}.inboxaddr{font-size:14px;color:#5f6368;font-weight:400;cursor:pointer;border-radius:6px;padding:3px 6px;margin:-3px -6px;transition:background .12s,color .12s}.inboxaddr:hover{background:#f2f3f5;color:#202124}.inboxaddr:focus-visible{outline:2px solid #1557b0;outline-offset:1px}.inboxaddr.copied{background:#e6f4ea;color:#137333}.inboxbar{display:flex;gap:10px;align-items:center;margin:10px 0 16px}.inboxbar .active{background:#e8eaed;border-color:#999;font-weight:700}.inboxbar .active:hover{background:#dde1e6;border-color:#777}.inboxbar .icon-btn{height:36px;width:36px;min-width:36px;padding:0;flex:0 0 auto;box-sizing:border-box}.bulkbar{display:flex;gap:8px;align-items:center;margin-left:auto}.select-banner{margin:0 0 10px;padding:8px 12px;background:#e8f0fe;border:1px solid #c6dafc;border-radius:8px;font-size:13px;color:#202124}.select-banner[hidden]{display:none}.select-banner b{font-variant-numeric:tabular-nums}.mailheader{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;margin:0 -16px;padding:0 12px 8px;color:#5f6368;font-size:12px;text-transform:uppercase;letter-spacing:.04em;border-bottom:1px solid #e5e5e5}.mailheader>span{text-align:left}.mailheader>span.hcenter{text-align:center}.hcenter{text-align:center}.mailrows{margin:0 -16px -16px}.mailrow{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;border-bottom:1px solid #eee;background:#f2f3f5;padding:0 12px}.mailrow:last-child{border-bottom:0}.mailrow.unread{background:#fff}.mailcheck{display:flex;align-items:center;justify-content:center}.mailcheck input[type=checkbox]{width:15px;height:15px;margin:0}.mailrowlink{grid-column:2 / 7;display:grid;grid-template-columns:22px minmax(150px,220px) 1fr 110px 84px;gap:8px;align-items:center;padding:12px 0;text-decoration:none;color:#5f6368;min-width:0}.mailrow.unread .mailrowlink{color:#202124}.mailrow.unread .mailsender,.mailrow.unread .mailsubject{font-weight:700}.mailsender,.mailsubject,.mailsnippet,.maildate,.mailsize{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.mailsnippet{color:#5f6368;font-weight:400}.maildate,.mailsize{font-size:13px;color:#5f6368;text-align:center}.mailrow.unread .maildate,.mailrow.unread .mailsize{color:#202124}.mailaction{grid-column:7;display:flex;justify-content:center;align-items:center}.mailaction form{margin:0}.mailaction .btn-sm{margin:0 2px}.mailrow.outbox{grid-template-columns:22px minmax(150px,220px) 1fr 110px 150px}.mailrow.outbox .mailrowlink{grid-column:1 / 5;grid-template-columns:22px minmax(150px,220px) 1fr 110px}.mailrow.outbox .mailaction{grid-column:5;justify-content:flex-end;gap:6px}.mailheader.drafts{grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 250px}.mailrow.drafts{grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 250px}.mailrow.drafts .mailrowlink{grid-column:2 / 6;grid-template-columns:22px minmax(150px,220px) 1fr 110px}.mailrow.drafts .mailaction{grid-column:6;justify-content:flex-end;gap:6px}.mailaction button{width:112px;text-align:center}.maildot{display:inline-block}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#1557b0}.unread-pill{background:#1557b0;color:#fff;font-size:13px;font-weight:600;padding:4px 13px;min-width:20px;text-align:center;font-variant-numeric:tabular-nums}.pending-pill{background:#1a73e8;color:#fff;font-size:13px;font-weight:600;padding:4px 13px;min-width:20px;text-align:center;text-decoration:none;font-variant-numeric:tabular-nums}.banner{padding:10px 12px;border-radius:8px;margin-bottom:16px;border:1px solid}.banner.warn{background:#fff8e1;border-color:#e6c34a}.banner.danger{background:#fdecef;border-color:#e0a0aa;color:#b00020}.inbox-flags{width:56px;text-align:center;vertical-align:middle!important}.inbox-flags>span,.inbox-flags>a{display:inline-flex;align-items:center;justify-content:center;width:20px;height:20px;margin:0 2px;vertical-align:middle}.catchall-tag{display:inline-flex;align-items:center;justify-content:center;width:15px;height:15px;vertical-align:-2px;color:#5f6368}.catchall-tag svg{width:13px;height:13px;display:block}.inbox-flags>span[style]{color:#5f6368}.inbox-flags>span svg{width:14px;height:14px}.draft-status{display:inline-flex;align-items:center;gap:5px;border-radius:999px;padding:4px 9px;font-size:13px;font-weight:600;line-height:1.1;white-space:nowrap}.draft-status svg{width:15px;height:15px;display:block}.draft-status.pending{background:#fff8e1;color:#8a6d00;border:1px solid #e6c34a}.draft-status.rejected{background:#fee;color:#b00020;border:1px solid #e99}.draft-status.sent{background:#eee;color:#333;border:1px solid #ccc}.mailframe{width:100%;flex:1 1 auto;min-height:360px;border:1px solid #ddd;border-radius:8px;background:#fff}.mail-reader>.msgbody{flex:1 1 auto;min-height:0}.attachments{list-style:none;padding:0;margin:8px 0}.attachments li{padding:4px 0}.brand{color:#202124;text-decoration:none}.brand-logo{display:block;height:44px;width:auto;max-width:min(44vw,240px);object-fit:contain}.org{font-weight:600;font-size:16px;color:#202124;max-width:40vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.card-head{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:8px}.card-head h2{margin:0}.small{font-size:13px}.icon-btn{height:30px;padding:0 7px;line-height:1;display:inline-flex;align-items:center;justify-content:center}.icon-btn svg{width:16px;height:16px;display:block}.dialog-actions{display:flex;justify-content:flex-end;align-items:center;gap:8px;margin-top:16px}.dialog-actions>form{margin:0;margin-right:auto}.dialog-danger{display:flex;gap:8px;align-items:center;margin-right:auto}.email-field{display:flex;align-items:stretch;margin:4px 0 10px}.email-field input,.email-field select{width:auto;margin:0}.email-field input{flex:1;border-radius:6px 0 0 6px}.email-field .at{display:flex;align-items:center;padding:0 8px;color:#666;background:#f2f3f5;border:1px solid #bbb;border-left:0;border-right:0}.email-field select{border-radius:0 6px 6px 0;border-left:0;max-width:50%}.row-link{cursor:pointer}.row-link:hover td{background:#f6f9ff}.key-fields[data-type=api]>label:first-child{display:flex;align-items:center;gap:8px;margin:4px 0 10px}.key-fields[data-type=api]>label:first-child input{width:auto;margin:0}.key-matrix{width:100%;margin:6px 0}.key-matrix th,.key-matrix td{padding:6px 8px;border-bottom:1px solid #eee;vertical-align:middle}.key-matrix td:last-child,.key-matrix th:last-child{text-align:right}.key-matrix th:last-child{white-space:nowrap}.key-matrix tr.domain-row td{background:#f7f8fa;font-weight:600}.key-matrix tbody[data-domain]+tbody[data-domain] tr:first-child td{border-top:2px solid #e0e0e0}.key-matrix td.domain-inbox{padding-left:20px}.key-matrix .key-matrix-domainsend button{min-width:104px}.key-matrix-domainsend-spacer{visibility:hidden;border-color:transparent;width:106px}.key-matrix-domainsend-spacer label{display:none}.seg{position:relative;display:inline-flex;border:1px solid #bbb;border-radius:7px;overflow:hidden;background:#fff}.seg input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0}.seg label{display:inline-block;min-width:92px;text-align:center;box-sizing:border-box;margin:0;padding:5px 11px;font-size:13px;line-height:1.2;color:#333;cursor:pointer;user-select:none;border-left:1px solid #ddd}.seg label:first-of-type{border-left:0}.seg input:checked+label{background:#111;color:#fff}.seg input:focus-visible+label{outline:2px solid #1557b0;outline-offset:-2px}.seg input:disabled+label{cursor:not-allowed}.seg button{border:0;border-left:1px solid #ddd;border-radius:0;background:#fff;color:#333;min-width:92px;text-align:center;box-sizing:border-box;font-weight:700;padding:5px 11px;font-size:13px;line-height:1.2}.seg button:first-of-type{border-left:0}.seg button:hover{background:#f2f3f5;color:#333}.seg button:disabled{color:#999}.seg button.active{background:#111;color:#fff}.seg button.active:hover{background:#000;color:#fff}.role-legend{width:100%;margin-top:12px;font-size:13px}.role-legend th,.role-legend td{padding:5px 8px;border-bottom:1px solid #eee;text-align:left}.role-legend td:first-child{white-space:nowrap;font-weight:600}.amber{background:#fff8e1;color:#8a6d00;border-color:#e6c34a}.pill.danger{background:#fee;color:#b00020;border-color:#e99}.size-near{color:#8a6d00;font-weight:600}.size-over{color:#b00020;font-weight:600}button.amber:hover{background:#fdf0c8;border-color:#c9a52f}#key-rotate{margin-right:auto}button:disabled{opacity:.4;cursor:not-allowed}button:disabled:hover{background:#111;border-color:#111}.notice{position:fixed;top:16px;left:50%;transform:translateX(-50%);z-index:100;max-width:min(92vw,560px);box-shadow:0 6px 20px rgba(0,0,0,.15);cursor:pointer;animation:notice-in .2s ease-out}.notice.dismissing{animation:notice-out .3s ease-in forwards}@keyframes notice-in{from{opacity:0;transform:translate(-50%,-8px)}to{opacity:1;transform:translate(-50%,0)}}@keyframes notice-out{to{opacity:0;transform:translate(-50%,-8px)}}@media (prefers-reduced-motion:reduce){.notice{animation:none}.notice.dismissing{animation:none;opacity:0}}.tab{padding:10px 18px;text-decoration:none;color:#111;background:#fff;border:1px solid #bbb;border-radius:6px;font-size:15px;font-weight:600;line-height:1.2;cursor:pointer}.tab:hover{background:#f2f3f5;border-color:#999;color:#111}.tab.active{background:#111;border-color:#111;color:#fff}.tab.active:hover{background:#000;border-color:#000;color:#fff}.steps{margin:8px 0 16px;padding-left:20px}.dkim-panel{margin-top:12px;padding:12px;border:1px solid #e3e3e3;border-radius:8px;background:#fafafa}.dkim-panel .section-head{margin:0 0 6px}.dkim-panel .dkim-head{display:flex;align-items:baseline;justify-content:flex-start;gap:10px;flex-wrap:wrap}.dkim-panel .dkim-head .section-head{margin:0 0 6px}.dkim-panel .dkim-status{display:flex;align-items:center;gap:2px;margin:0 0 6px;font-size:13px}.dkim-panel .dkim-status .dkim-status-text{font-weight:600}.dkim-panel .dns-remediation-row{align-items:center;flex-wrap:wrap;margin:6px 0}.dkim-panel .dns-remediation-row .dns-remediation-label{flex:0 0 52px}.dkim-panel .dns-remediation-value{display:flex;align-items:center;overflow:visible;white-space:pre-wrap;overflow-wrap:anywhere;word-break:break-word;max-height:none;align-self:center}.linklike{background:none;border:0;padding:0;color:#1557b0;font-size:13px;cursor:pointer;text-decoration:underline}.linklike:hover{background:none;border:0;color:#0b3d91}.dkim-reveal{margin:10px 0 0}.steps li{margin:6px 0}.steps code{overflow-wrap:anywhere}.setup-notes{margin:10px 0 0;padding-left:18px;font-size:13px;line-height:1.5}.setup-notes li{margin:3px 0}.cf-code{max-height:420px;overflow:auto}.domains-table{table-layout:fixed}.domains-table th,.domains-table td{padding:4px 8px;line-height:1.3;vertical-align:middle;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.domains-table td:first-child b{font-weight:400;white-space:normal;overflow-wrap:anywhere;word-break:break-word}.domains-table th:first-child,.domains-table td:first-child{width:32%;padding-left:8px}.domains-table th:nth-child(2),.domains-table td:nth-child(2){width:14%}.domains-table th:nth-child(3),.domains-table td:nth-child(3),.domains-table th:nth-child(4),.domains-table td:nth-child(4){width:18%}.domains-table th:last-child,.domains-table td:last-child{width:18%}.domains-table .cell-edit{min-width:0;width:100%;max-width:100%;height:26px;min-height:26px;display:inline-flex;align-items:center;justify-content:center;padding:0 4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;line-height:1.2}.domains-table .domain-catchall-add{width:auto;max-width:100%;height:26px;min-height:26px;padding:0 7px}.domains-table .domain-catchall-link{display:inline-flex;align-items:center;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.domains-table .domain-provider-edit{height:26px;min-height:26px;font-size:12px;line-height:1.2;padding:0 4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.domains-table .domain-actions{display:flex;width:max-content;max-width:100%;box-sizing:border-box;align-items:center;justify-content:flex-start;gap:4px;flex-wrap:nowrap}.domains-table .domain-actions .icon-btn{flex:0 0 26px;width:26px;height:26px;padding:0}.domains-table .domain-receiving{display:flex;align-items:center;gap:6px}.domains-table .domain-provider-edit .dns-light{width:8px;height:8px;min-width:8px;flex:0 0 8px;padding:0;border:0;margin:0 5px 0 0;cursor:help}.cell-edit .muted{color:#666}.inherited{color:#9aa0a6;font-size:12px}.cell-link{background:none;border:0;padding:0;margin:0;font-size:13px;color:#1557b0;cursor:pointer;text-align:left}.cell-link:hover{background:none;border:0;text-decoration:underline}.provider-select{max-width:320px}.provider-hint{margin:0}.inherit-option{display:flex;align-items:flex-start;gap:8px;margin:6px 0;width:auto;min-width:0;text-align:left}.inherit-option input[type=checkbox]{width:15px;height:15px;margin:2px 0 0;flex:0 0 auto}.inherit-option span{min-width:0}.provider-box{border:1px solid #ddd;border-radius:8px;padding:12px 12px 4px;margin-top:8px;background:#fafafa}.domain-dialog{width:640px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}.domain-dialog--wide{width:min(960px,calc(100vw - 48px))}.domain-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0;margin-top:8px}dialog .dialog-head{display:flex;align-items:flex-start;justify-content:space-between;gap:12px;margin-bottom:10px}.confirm-name{cursor:pointer;border-radius:4px;padding:1px 4px;margin:-1px -4px;transition:background .12s,color .12s}.confirm-name:hover{background:#f2f3f5}.confirm-name:focus-visible{outline:2px solid #1557b0;outline-offset:1px}.confirm-name.copied{background:#e6f4ea;color:#137333}.copy-btn.copied{background:#e6f4ea;border-color:#b7e1c4;color:#137333}.confirm-inbox-list{max-height:160px;overflow:auto;margin:6px 0;white-space:pre-line;font-size:13px;color:#5f6368}.attach-drop{border:2px dashed #bbb;border-radius:8px;padding:16px;margin:4px 0 10px;background:#fafafa;text-align:center;transition:border-color .15s,background .15s}.attach-drop.dragover{border-color:#1557b0;background:#eef4fd}.attach-hint{margin:0 0 8px;color:#5f6368;font-size:13px}.attach-browse{cursor:pointer}.attach-input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap}.attach-list{list-style:none;margin:10px 0 0;padding:0;text-align:left}.attach-list li{display:flex;align-items:center;gap:10px;padding:6px 8px;border:1px solid #e3e3e3;border-radius:6px;background:#fff;margin-bottom:6px}.attach-list .attach-name{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.attach-list .attach-size{color:#5f6368;font-size:12px;white-space:nowrap}.attach-list .attach-remove{border:0;background:none;color:#5f6368;font-size:18px;line-height:1;padding:0 4px;cursor:pointer}.attach-list .attach-remove:hover{background:none;color:#b00020}.attach-overlay{display:none;position:fixed;inset:0;z-index:200;align-items:center;justify-content:center;background:rgba(21,87,176,.10);border:3px dashed #1557b0;box-sizing:border-box}.attach-overlay.active{display:flex}.attach-overlay-inner{background:#fff;border:1px solid #1557b0;border-radius:10px;padding:16px 28px;font-size:18px;font-weight:600;color:#1557b0;box-shadow:0 8px 24px rgba(0,0,0,.12)}.labelpill{display:inline-flex;align-items:center;gap:4px;background:#e6f4ea;color:#137333;border:1px solid #b7e1c4;border-radius:999px;padding:1px 8px;font-size:11px;vertical-align:middle}.labelpill span{overflow:hidden;text-overflow:ellipsis}.labelx{border:0;background:none;color:#137333;font-size:13px;line-height:1;padding:0 2px;cursor:pointer;box-shadow:none}.labelx:hover{background:none;color:#b00020}.msgmeta{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;flex-wrap:wrap}.msgmeta p{margin:0}.labelbar{display:flex;gap:6px;flex-wrap:wrap;align-items:center;margin:0 0 10px}.labelbar form{margin:0}.labeladd{display:flex;gap:4px;align-items:center;flex:0 0 auto}.labeladd input{width:min(220px,48vw);margin:0;padding:5px 10px;line-height:1.35;border-radius:7px}.labelbar .btn-sm{padding:2px 8px;font-size:12px}.moveto{display:flex;gap:4px;align-items:center;flex:0 0 auto}.moveto select{margin:0;max-width:200px}.dialog-tabs{display:flex;gap:6px;flex-wrap:wrap;margin:0 0 14px;padding-bottom:10px;border-bottom:1px solid #eee}.dialog-tab{background:#fff;color:#202124;border:1px solid #bbb;border-radius:999px;padding:5px 13px;font-size:13px;line-height:1.3}.dialog-tab:hover{background:#f2f3f5}.dialog-tab.active{background:#111;color:#fff;border-color:#111}.dialog-panel{margin:0}.dialog-usage{display:grid;grid-template-columns:minmax(90px,max-content) 1fr;gap:4px 12px;margin:8px 0;font-size:14px}.dialog-usage dt{color:#666}.dialog-usage dd{margin:0}.section-head{margin:14px 0 6px;font-size:14px;font-weight:700}.dialog-panel>.section-head:first-child{margin-top:0}.slist.aliases .alias-row{display:flex;align-items:center;gap:8px}.slist .alias-text{flex:1;min-width:0;display:flex;flex-direction:column;gap:1px}.slist .alias-row-name{font-size:14px;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist .alias-row-name.muted{font-weight:400}.slist .alias-row-addr{font-size:12px;color:#5f6368;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist.passkey-list .alias-row-name{flex:1;min-width:0}#alias-dialog{width:420px;max-width:calc(100vw - 24px)}.mail-layout{display:grid;grid-template-columns:210px minmax(0,1fr);gap:20px;align-items:start;flex:1 1 auto;min-height:0}.mailcontent{display:flex;flex-direction:column;min-width:0;min-height:0;align-self:stretch}.card.mail-reader{flex:1 1 auto;min-height:0}.mailnav{position:sticky;top:16px;display:flex;flex-direction:column;gap:4px}.mailnav .compose{width:100%;justify-content:center;margin-bottom:8px}.mailnav a.folder{display:flex;align-items:center;justify-content:space-between;gap:8px;padding:8px 12px;border-radius:8px;color:#202124;text-decoration:none;font-size:15px}.mailnav a.folder:hover{background:#f2f3f5}.mailnav a.folder.active{background:#e8eaed;font-weight:700}.mailnav .count{color:#5f6368;font-size:13px;font-weight:400;font-variant-numeric:tabular-nums}.mailnav .count.unread{background:#1557b0;color:#fff;border-radius:999px;padding:1px 8px}.mailnav .count.unread.stale{opacity:.55;font-style:italic}.mailnav .navgroup{margin:10px 0 2px;padding:0 12px;font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.04em;color:#5f6368}.mailnav a.folder.label{padding-left:24px;font-size:14px}.mailnav a.folder.label .labelname{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.mailnav .folder-row{display:flex;align-items:center;gap:2px}.mailnav .folder-row a.folder{flex:1;min-width:0}.mailnav .folder-manage{flex:0 0 auto;min-width:30px;min-height:30px;display:inline-flex;align-items:center;justify-content:center;border:0;background:none;color:#9aa0a6;padding:5px;border-radius:6px;cursor:pointer;line-height:1}.mailnav .folder-manage:hover{background:#e8eaed;color:#202124}.mailnav .folder-manage svg{width:14px;height:14px;display:block}.mailnav .navgroup-head{display:flex;align-items:center;justify-content:space-between;gap:8px;margin:12px 0 3px}.mailnav .navgroup-new{padding:0;width:32px;height:32px;border-radius:6px;border:0;background:transparent;color:inherit;cursor:pointer;font-size:19px;font-weight:500;line-height:1}.mailnav .navgroup-new:hover{background:#e8eaed}.mailnav .navgroup-new:focus-visible,.mailnav .folder-manage:focus-visible,.mailnav .folder-row a.folder:focus-visible{outline:2px solid #1557b0;outline-offset:2px}.mailnav .label-location{font-size:12px;opacity:.55;margin-left:auto}.mailnav .folder-row a.folder.label{display:flex;align-items:center;justify-content:space-between}.mailnav .newfolder{width:100%;justify-content:center;margin-top:6px}.folder-dialog{width:420px;max-width:calc(100vw - 24px)}@media (max-width:760px){.mail-layout{grid-template-columns:1fr;gap:12px}.mailnav{position:static;flex-direction:row;flex-wrap:wrap;align-items:center;border-bottom:1px solid #e5e5e5;padding-bottom:8px}.mailnav .compose{width:auto;margin:0 4px 0 0}.mailnav a.folder{padding:6px 10px}.mailnav .navgroup-head{flex:0 0 100%;border-top:1px solid #e5e5e5;margin:8px 0 0;padding:8px 8px 0}.mailnav .folder-row{flex:1 1 100%;width:100%}.mailnav .folder-row a.folder.label{padding-left:10px}.mailnav .folder-manage{margin-right:4px}}.mailaction button{width:112px;text-align:center}.mailaction button.icon-btn{width:30px;padding:0}.mailaction .icon-btn{margin:0 2px}.mailaction .icon-btn svg{width:18px;height:18px}@media (max-width:640px){body{padding:16px}.mailheader{display:none}.mailrow,.mailrow.outbox,.mailrow.drafts{grid-template-columns:26px 1fr;column-gap:8px;row-gap:0;padding:10px 12px}.mailrow.outbox{grid-template-columns:1fr}.mailrow .mailcheck{grid-column:1}.mailrow .maildot{display:none}.mailrow .mailrowlink,.mailrow.outbox .mailrowlink,.mailrow.drafts .mailrowlink{grid-column:2;display:grid;grid-template-columns:1fr auto;column-gap:8px;align-items:center;padding:0;min-width:0}.mailrow.outbox .mailrowlink{grid-column:1}.mailrow .mailsender{display:none}.mailrow .mailsnippet{display:none}.mailrow .mailsize{display:none}.mailrow .mailsubject{grid-column:1;font-size:14px}.mailrow .maildate{grid-column:2;text-align:right;font-size:12px;white-space:nowrap}.mailrow .mailaction{display:none}.mailrow.outbox .mailaction{display:flex;grid-column:1;justify-content:flex-end;margin-top:6px}.mailrow.outbox .maildate{grid-column:2;grid-row:1}.mailrow.outbox .mailsubject{grid-column:1}.mailrow.outbox .mailsender{grid-column:1;grid-row:2;display:block;font-size:12px}.mailrow.drafts .mailaction{display:flex;grid-column:1 / -1;justify-content:flex-end;margin-top:6px}.mailrow.drafts .mailsubject{grid-column:1}.mailrow.drafts .maildate{grid-column:2;grid-row:1}.mailrow.drafts .mailsender{grid-column:1;grid-row:2;display:block;font-size:12px}.msghead{grid-template-columns:minmax(0,1fr)}.msghead .actions{grid-column:1;justify-self:start;flex-wrap:wrap}.inboxhead,.inboxbar,.toolbar,.bulkbar{flex-wrap:wrap}.inboxbar .bulkbar{margin-left:0}.card{padding:12px}.grid,.dashboard-grid{grid-template-columns:1fr}.search-form{flex-wrap:wrap}.provider-picker select{max-width:none}.domains-table{table-layout:auto}.quota{display:none}.menu,.menu-right{gap:8px}h1{font-size:1.5em}.inboxtitle{font-size:1.5em}}#key-dialog{max-height:calc(100dvh - 32px);overflow:hidden}#key-form{display:flex;flex-direction:column;min-height:0;max-height:calc(100dvh - 96px)}#key-matrix{min-height:0;overflow-y:auto;overscroll-behavior:contain}#key-matrix .key-matrix{margin:0}.key-dialog-actions{flex:0 0 auto;padding-top:10px;margin-top:4px;background:white;border-top:1px solid #eee}.dialmx-dns{list-style:none;margin:8px 0;padding:0}.dialmx-dns li{margin:6px 0;line-height:1.5}span.dns-light{display:inline-block;width:10px;height:10px;min-width:10px;padding:0;border:0;border-radius:50%;vertical-align:middle;background:#9aa0a6}span.dns-light.ok{background:#137333}span.dns-light.amber{background:#e6c34a}.dns-light.grey{background:#9aa0a6}span.dns-light.danger{background:#b00020}.dialmx-status dt{font-weight:600;margin-top:8px}.dialmx-status dd{margin:0}.setting-row{display:grid;grid-template-columns:minmax(160px,300px) minmax(0,1fr);gap:12px 24px;align-items:start;padding:14px 0;border-bottom:1px solid #eee}.setting-row:last-child{border-bottom:0}.setting-row .setting-label label{display:block;min-width:0;text-align:left;padding:0;border:0;font-size:14px;font-weight:600;color:#202124}.setting-row .setting-label .muted{margin:2px 0 0}.setting-row input,.setting-row select{margin:0;width:auto;min-width:0;justify-self:start}.setting-row input[type=text],.setting-row input[type=email],.setting-row input[type=password],.setting-row input[type=number],.setting-row input[type=url],.setting-row input:not([type]),.setting-row select{width:100%;justify-self:stretch}.setting-row input[list]{width:100%;max-width:22rem;justify-self:start}@media(max-width:680px){.setting-row{grid-template-columns:1fr;gap:6px}.setting-row input[list]{max-width:none}}/* Combobox: a real dropdown attached to a text input, replacing the native
datalist popup. The list is a normal bordered menu that filters as you type. */
.combo-wrap{position:relative;display:block;min-width:0}.combo-wrap>input{width:100%;margin:0}.combo-list{position:absolute;z-index:70;left:0;right:0;top:calc(100% + 2px);margin:0;padding:4px;list-style:none;max-height:min(240px,40vh);overflow-y:auto;background:#fff;border:1px solid #bbb;border-radius:8px;box-shadow:0 6px 20px rgba(32,33,36,.16);box-sizing:border-box}.combo-list[hidden]{display:none}.combo-option{padding:6px 10px;border-radius:6px;font-size:14px;line-height:1.3;cursor:pointer;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.combo-option:hover,.combo-option.active{background:#e8f0fe;color:#174ea6}.combo-empty{padding:6px 10px;font-size:13px;color:#5f6368}.conn-status{display:inline-flex;align-items:center;gap:6px;font-size:13px;color:#5f6368;white-space:nowrap;padding:0 4px}.conn-status .conn-dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#9aa0a6;flex:0 0 auto}.conn-status[data-state=live] .conn-dot{background:#137333}.conn-status[data-state=disconnected] .conn-dot{background:#b00020}.conn-status[data-state=error] .conn-dot{background:#e6c34a}.conn-status[data-state=loading] .conn-dot{width:12px;height:12px;background:none;border:2px solid #b8c9e8;border-top-color:#174ea6;animation:mail-conn-spin .8s linear infinite}@keyframes mail-conn-spin{to{transform:rotate(360deg)}}@media (prefers-reduced-motion:reduce){.conn-status[data-state=loading] .conn-dot{animation:none}}@media(max-width:640px){.conn-status .conn-label{display:none}}</style></head><body{{if .Page}} data-page="{{.Page}}"{{if .Inbox}} data-inbox="{{.Inbox.ID}}"{{if .Folder}} data-folder="{{.Folder}}"{{end}}{{if .ActiveLabel}} data-label="{{.ActiveLabel}}"{{end}}{{end}}{{end}}><header><div class="brandrow"><a class="brand" href="/" title="Home" aria-label="Mammoth Mail home"><img class="brand-logo" src="{{asset "logo-horizontal.png"}}" alt="Mammoth Mail"></a>{{if .Account.ID}}<span class="org">{{.Account.Name}}</span>{{end}}</div>{{if .Principal.UserID}}<nav class="menu"><div class="menu-right">{{if or (eq .Page "dashboard") (eq .Page "inbox")}}<span class="conn-status" data-conn-status data-state="connecting" role="status" aria-live="polite"><span class="conn-dot" aria-hidden="true"></span><span class="conn-label">Connecting…</span></span>{{end}}{{if .Account.ID}}<span class="quota">{{filesize .Account.StorageUsedBytes}} of {{filesize .Account.StorageQuotaBytes}} stored.</span>{{end}}<a class="tab{{if or (eq .Tab "account") (eq .Tab "admin")}} active{{end}}" id="settings-open" href="/account" aria-haspopup="dialog">Settings</a><button type="button" class="tab" id="feedback-open" aria-haspopup="dialog">Feedback</button><form method="post" action="/logout"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="tab">Log Out</button></form></div></nav>{{end}}</header><dialog id="feedback-dialog" class="domain-dialog domain-dialog--wide" aria-labelledby="feedback-title"><div class="dialog-head"><h2 id="feedback-title">Mammoth Mail is in active development</h2><button type="button" class="btn secondary" data-close-dialog>Back</button></div><p>Bug reports, feature ideas, and rough edges all shape what comes next.</p><p>Email us at <a href="mailto:{{feedbackEmail}}">{{feedbackEmail}}</a> — or open an issue or pull request at <a href="{{feedbackRepo}}" target="_blank" rel="noopener noreferrer">{{feedbackRepo}}</a>.</p><div class="dialog-actions"><a class="btn secondary" href="mailto:{{feedbackEmail}}" id="feedback-mail">Open email</a><a class="btn secondary" href="{{feedbackRepo}}" target="_blank" rel="noopener noreferrer" id="feedback-github">Open GitHub</a></div></dialog>{{if .ShowOneReconnect}}<dialog id="one-reconnect-dialog" class="domain-dialog"><h2>Reconnect with One</h2><p class="muted">This inbox connects to Gmail through your One account. Save a valid One API key to restore the connection. The key is verified against this inbox's original Gmail identity before it is stored.</p><form id="one-reconnect-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="inbox_id" id="one-reconnect-inbox"><label>One API key</label><input name="api_key" id="one-reconnect-key" type="text" required autocomplete="off" spellcheck="false" autocapitalize="off" maxlength="4096"><p class="muted small">Need a key? Connect Gmail in <a href="https://app.withone.ai/" target="_blank" rel="noopener noreferrer">One &rarr; Connected Apps</a>, then generate a REST key under <b>Developers &rarr; API Keys</b>.</p><p class="muted small" id="one-reconnect-status" role="status" aria-live="polite"></p><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" id="one-reconnect-submit">Save and check</button></div></form></dialog>{{end}}{{template "body" .}}<script src="{{asset "app.js"}}" defer></script></body></html>` + inboxTableTemplate

// dialMXSetupView is the per-domain Dial MX setup panel: the public half of the
// domain's exact signing key (never the private seed), the DNS records the
// operator publishes, and the live per-receiver authentication state plus
// published-record checks as traffic lights. It is rendered for any domain
// whose effective receiving provider is Dial MX, including a subdomain that
// inherits Dial MX from an ancestor — such a domain still owns its own exact
// key even though its receiver list is inherited.
type dialMXSetupView struct {
	KeyID        string
	PublicKey    string
	TXT          string
	ReceiverURLs string
	Service      string
	ContactEmail string
	Enforcement  string
	// InheritedFrom names the ancestor whose receiving config this domain
	// inherits, or "" when the setup belongs to the domain itself. An inherited
	// setup is read-only here: its settings are edited on the owning domain.
	InheritedFrom string
	MX            []dialMXMXInstruction
	Statuses      []mxdialStatusView
	DNS           []domainDNSView
	// ConnectorsJSON is the secret-free connector hostname/priority set embedded
	// on the receiving form so the status wizard can draw its full connector
	// skeleton immediately, before the first status poll returns. It carries no
	// live state and no secret.
	ConnectorsJSON string
	// StatusesJSON is the secret-free live per-receiver status set embedded on the
	// receiving form, so the status wizard's connector table opens already
	// matching the dashboard light it was opened from rather than flashing an
	// amber "Pending" until the first status poll returns. It carries no secret.
	StatusesJSON string
	// InstructionsJSON is the copy-ready DNS records (Tusk MX/TXT names, values
	// and priorities) embedded on the receiving form, so the status view can draw
	// its fix prompts (an unauthorized TXT record, or a receiver missing from MX)
	// the instant it opens rather than waiting for the first fetch to return. It
	// carries no secret — the TXT value is the domain's public key already shown in
	// the panel.
	InstructionsJSON string
	// UnexpectedJSON is the watcher's shared extra-MX verdict embedded on the form,
	// so the status view opens already showing its "additional MX servers" warning
	// in step with the dashboard's amber light. It carries no secret.
	UnexpectedJSON string
	// DNSJSON is the watcher's shared published-record verdict (TXT + MX checks)
	// embedded on the form, so the status view can show a domain's DNS problems
	// per check on open — even when the receiver stopped at its first failing gate
	// and reports only one reason.
	DNSJSON string
	// ManifestJSON is where the domain's snapshotted Tusk MX receiver set came
	// from (source and fetch time) embedded on the form, so the status view states
	// it on open rather than waiting for the first fetch to land. It carries no
	// secret.
	ManifestJSON string
	// Light is the aggregate traffic light for the domain's receiving setup
	// ("ok", "amber" or "danger"), and LightTitle its tooltip. It is green only
	// when at least one receiver is both authorized and published in the domain's
	// MX records, so the dashboard answers "will inbound mail be accepted?" at a
	// glance without opening the dialog.
	Light      string
	LightTitle string
}

// mxSetupView is the per-domain Direct MX status panel: the installation
// receiver's mode and live state, and the SMTP hostname the domain should point
// its MX record at. It carries no secret and mirrors the system-admin receiver
// configuration read-only, so an account admin choosing Direct MX can see
// whether mail will actually be accepted without leaving this dialog.
type mxSetupView struct {
	Editors    map[string]mxReceiverEditorView
	Configured bool
	Mode       string
	State      string
	// TargetHost is the hostname to publish in the domain's MX record. For the
	// included receiver it is the configured (or public-URL) hostname, known
	// before the receiver is live; for a separately hosted receiver it is only
	// known from the live handshake.
	TargetHost string
}

// remoteMXSetupView is the account's Remote MX receiver panel: the redacted
// configuration, whether a bearer credential is stored, and the live session
// state. The bearer secret and private CA are never part of this view. It is
// shown to account admins in the Remote MX dialog and drives the receiver
// editor; non-admins see status only.
type remoteMXSetupView struct {
	URL           string
	KeyConfigured bool
	AllowPrivate  bool
	CA            string
	Revision      int64
	State         string
	Detail        string
	Configured    bool
	Error         string
}

// mxStateLabel renders the live receiver state for the Direct MX panel.
func mxStateLabel(state string) string {
	switch state {
	case app.MXStateActive:
		return "active"
	case app.MXStateConnecting:
		return "connecting"
	case app.MXStateStandby:
		return "standby"
	case app.MXStateDraining:
		return "draining"
	case app.MXStateFailed:
		return "failed"
	case app.MXStateUnavailable:
		return "unavailable"
	case app.MXStateDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// The Direct MX DNS target is Mammoth Mail's public receiving URL hostname, not
// the independently configurable SMTP greeting.
func (s *Server) mxTargetHost(st app.MXReceiverStatus) string {
	if st.Mode != app.MXModeIncluded {
		return st.SMTPAddr
	}
	if publicURL, err := url.Parse(s.Service.Config.ReceiverURL()); err == nil {
		return publicURL.Hostname()
	}
	return ""
}

// decodePublicKey decodes the canonical base64url public key stored for a Dial
// MX credential. The stored form is unchanged; it is only decoded for the TXT
// record.
func decodePublicKey(raw string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(raw)
	return b
}

// dialMXConnectorsJSON marshals the setup's MX connectors to the secret-free
// JSON embedded on the receiving form. The status wizard reads it to lay out the
// full connector table on open, before the first status poll returns. It carries
// only hostname and priority. html/template escapes the result in attribute
// context, so the browser decodes it back to valid JSON.
func dialMXConnectorsJSON(mx []dialMXMXInstruction) string {
	if len(mx) == 0 {
		return "[]"
	}
	b, err := json.Marshal(mx)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// dialMXStatusesJSON marshals the setup's live per-receiver statuses to the
// secret-free JSON embedded on the receiving form. The status wizard reads it to
// paint each connector's real light on open, before the first status poll
// returns, so the dialog agrees with the dashboard light that opened it.
// html/template escapes the result in attribute context, so the browser decodes
// it back to valid JSON.
func dialMXStatusesJSON(statuses []mxdialStatusView) string {
	if len(statuses) == 0 {
		return "[]"
	}
	b, err := json.Marshal(statuses)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// dialMXInstructionsJSON marshals the setup's copy-ready DNS records to the
// secret-free JSON embedded on the receiving form. The status view reads it to
// draw its fix prompts (an unauthorized TXT record, or a receiver missing from
// MX) on open, before the first fetch returns, so it never shows rows without
// their remediation. A nil instructions object (no copy-ready records) is
// marshalled as null so the client falls back to the fetched value.
func dialMXInstructionsJSON(instructions *dialMXDNSInstructions) string {
	if instructions == nil {
		return "null"
	}
	b, err := json.Marshal(instructions)
	if err != nil {
		return "null"
	}
	return string(b)
}

// antlerManifestJSON marshals the recorded Tusk manifest source to the
// secret-free JSON embedded on the receiving form, so the status view states
// whether setup used the live GitHub manifest or the built-in defaults as soon
// as it opens. A config with no record is marshalled as null, and the view
// renders no line rather than guessing.
func antlerManifestJSON(manifest *antlerManifestView) string {
	if manifest == nil {
		return "null"
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return "null"
	}
	return string(b)
}

// dialMXUnexpectedJSON marshals the watcher's shared extra-MX verdict to the
// secret-free JSON embedded on the receiving form, so the status view opens
// already showing its "additional MX servers" warning in step with the amber
// dashboard light.
func dialMXUnexpectedJSON(hosts []string) string {
	if len(hosts) == 0 {
		return "[]"
	}
	b, err := json.Marshal(hosts)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// dialMXDNSJSON marshals the watcher's shared published-record checks to the
// secret-free JSON embedded on the receiving form, so the status view opens
// already showing a domain's DNS problems per check. The records are public
// (they are what the operator publishes), so nothing secret is included.
func dialMXDNSJSON(views []domainDNSView) string {
	if len(views) == 0 {
		return "[]"
	}
	b, err := json.Marshal(views)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// dialMXStatuses snapshots the manager's live per-receiver status for a domain.
// A nil manager (Dial MX disabled) yields no statuses rather than an error.
func (s *Server) dialMXStatuses(domain string) []mxdialStatusView {
	if s.Service.DialMX == nil {
		return nil
	}
	live := s.Service.DialMX.Status(domain)
	out := make([]mxdialStatusView, 0, len(live))
	for _, st := range live {
		v := mxdialStatusView{ReceiverURL: st.ReceiverURL, State: st.State, Reason: boundStatusReason(st.Reason), SMTPHostname: st.SMTPHostname, HTTPCode: st.HTTPCode}
		if !st.ExpiresAt.IsZero() {
			t := st.ExpiresAt
			v.ExpiresAt = &t
		}
		out = append(out, v)
	}
	return out
}

// dialMXReady reports whether a receiver has accepted this domain's exact key.
// Only a ready receiver advertises a usable MX target.
func dialMXReady(st mxdialStatusView) bool { return st.State == mxdial.StatusActive }

// dialMXStatusClass returns a "danger" pill class for a receiver that is a
// settled failure — rejected (including not_mx), unavailable, or unreachable —
// and "" for the amber in-progress states, matching the JS dialog.
func dialMXStatusClass(st mxdialStatusView) string {
	switch st.State {
	case mxdial.StatusRejected, mxdial.StatusUnavailable, mxdial.StatusUnreachable:
		return "danger"
	default:
		return ""
	}
}

// dialMXStatusLabel renders one receiver row's state for the server-rendered
// setup panel. It mirrors the wizard's labels so the dialog and the panel never
// describe the same receiver differently: the receiver's own reason wins where
// it names a settled refusal, and a ready row whose authorization window has
// closed is expired rather than in progress.
func dialMXStatusLabel(st mxdialStatusView) string {
	switch st.Reason {
	case "session_rejected":
		return "receiver refused the session"
	case "receiver_error":
		return "receiver reported a server error"
	case "http2_required":
		return "receiver speaks HTTP/1.1, not HTTP/2"
	case "mode_mismatch":
		return "wrong receiver mode"
	case "protocol_mismatch":
		return "receiver protocol mismatch"
	case "destination_not_allowed":
		return "blocked — receiver destination not allowed"
	case "replaced":
		return "replaced by another connection"
	}
	switch st.State {
	case mxdial.StatusActive:
		if st.ExpiresAt != nil && !st.ExpiresAt.After(time.Now()) {
			return "authorization expired"
		}
		return "ready"
	case mxdial.StatusConnecting:
		return "authorizing"
	case mxdial.StatusRejected:
		if st.Reason == "not_mx" {
			return "not in MX"
		}
		return "rejected"
	case mxdial.StatusUnreachable:
		return "receiver unreachable"
	case mxdial.StatusUnavailable:
		return "unavailable"
	case mxdial.StatusDeferred:
		return "waiting to retry"
	case mxdial.StatusDisconnected:
		return "reconnecting"
	}
	return "waiting"
}

// dnsLight reports the traffic-light token for one published-record check on the
// receiving side. A settled record state is a hard gate: green only when the
// record is published and matches, red otherwise. "not_found" (nothing published)
// and "dns_error" (the lookup failed) are red as well, because the operator must
// not proceed on either — "we cannot confirm this record" is not a licence to
// continue. Amber is reserved for the receiver's extra-MX warning, which is a
// correct DNS config with ambiguous routing, and for a check that has not run
// yet; neither is a per-record verdict.
func dnsLight(v domainDNSView) string {
	switch v.State {
	case app.StateOK:
		return "ok"
	default:
		return "danger"
	}
}

// dkimLight reports the traffic-light token for a Direct MX signing domain's
// published DKIM record. Unlike the receiving checks, an absent DKIM record is
// not a failure: mail still sends, it just does not get the enforcement DKIM
// gives it. So "not_found" and "dns_error" are grey (not confirmed), while only a
// published record that does not match the signing key is red. It returns "" when
// there is no DKIM check (signing off), so no light renders.
func dkimLight(v *domainDNSView) string {
	if v == nil {
		return ""
	}
	switch v.State {
	case app.StateOK:
		return "ok"
	case app.StateMismatch:
		return "danger"
	default:
		return "grey"
	}
}

// dkimStatusText names the published-DKIM-record state in words, so the sending
// dialog can show a status line beside the dot rather than a bare circle. It
// takes the light token dkimLight produced and returns "" for no check.
func dkimStatusText(light string) string {
	switch light {
	case "ok":
		return "DNS valid"
	case "danger":
		return "DNS invalid"
	case "grey":
		return "DNS not present"
	default:
		return ""
	}
}

// domainLight is the light + tooltip pair a dashboard cell renders. It is shared
// by the receiving light (dialMXHealth) and the sending DKIM light.
type domainLight struct {
	Light string `json:"light"`
	Title string `json:"title"`
}

// render renders body with timestamp helpers bound to the request's effective
// display zone, so the human UI shows local time without changing stored or API
// timestamps.
func (s *Server) render(w http.ResponseWriter, r *http.Request, body string, data any) {
	s.renderExtra(w, r, "", body, data)
}

// renderExtra is render with additional named templates parsed alongside the
// page shell. They must be siblings of the body define, not part of it, so the
// extra definitions are appended after the shell's own defines and before the
// body define wraps the page body.
func (s *Server) renderExtra(w http.ResponseWriter, r *http.Request, extra, body string, data any) {
	t, err := template.New("page").Funcs(s.templateFuncs(requestTZ(r))).Parse(pageTemplate + `{{define "mx-editor"}}` + mxReceiverSection + `{{end}}{{define "account-operators"}}` + accountOperatorsSection + `{{end}}` + extra + `{{define "body"}}` + body + `{{end}}`)
	if err != nil {
		s.Log.Error("template parse failed", "error", err)
		http.Error(w, "could not render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Session pages carry mailbox content: never let them land in any cache
	// (shared, disk, or back/forward cache), and vary on the session cookie so
	// a cache cannot collapse two users' views to one entry.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Cookie")
	if err = t.Execute(w, data); err != nil {
		s.Log.Error("render", "error", err)
	}
}

// renderMail renders a mailbox page. It is render plus the named sub-templates
// the mailbox body references (the message list and the send-requests card), so
// the full page and the live fragments share exactly one definition of each.
func (s *Server) renderMail(w http.ResponseWriter, r *http.Request, body string, data any) {
	s.renderExtra(w, r, selectBanner+liveRequestsCard+liveListCard, body, data)
}

// renderFragment renders one named sub-template on its own, without the page
// shell. The mailbox live-update endpoint uses it to return just the message
// list or the send-requests card so the browser can swap that fragment in place
// without re-rendering (or scrolling) the whole page. The template set is the
// same one the full inbox page is parsed with, so a fragment and the page it
// replaces can never drift.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, templateName string, data any) {
	t, err := template.New("frag").Funcs(s.templateFuncs(requestTZ(r))).Parse(liveRequestsCard + liveListCard)
	if err != nil {
		s.Log.Error("fragment parse failed", "error", err)
		http.Error(w, "could not render fragment", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err = t.ExecuteTemplate(w, templateName, data); err != nil {
		s.Log.Error("render fragment", "error", err)
	}
}

// templateFuncs builds the template helper set. Formatting helpers close over
// loc so they can render UTC timestamps in the viewer's zone.
func (s *Server) templateFuncs(loc *time.Location) template.FuncMap {
	if loc == nil {
		loc = time.UTC
	}
	return template.FuncMap{
		"bytes":                formatBytes,
		"join":                 strings.Join,
		"snippet":              snippetText,
		"mailDate":             func(t time.Time) string { return formatMailDate(loc, t) },
		"lastUsedDate":         func(t time.Time) string { return lastUsedDate(loc, t) },
		"passkeyBackup":        passkeyBackup,
		"localDateTime":        func(t time.Time) string { return formatLocalDateTime(loc, t) },
		"filesize":             filesize,
		"shortURL":             shortTooltipURL,
		"inboxSizeClass":       inboxSizeClass,
		"inboxSizeTitle":       inboxSizeTitle,
		"asset":                s.assetURL,
		"noindex":              func() bool { return s.Service.Config.NoIndex },
		"linkify":              linkifyText,
		"querystring":          url.QueryEscape,
		"aliasNames":           aliasNamesCSV,
		"connectorsJSON":       connectorsJSON,
		"inboxesJSON":          inboxesJSON,
		"copyIcon":             func() template.HTML { return template.HTML(iconCopy) },
		"catchAllIcon":         func() template.HTML { return template.HTML(catchAllTag) },
		"deliveryStatus":       deliveryStatus,
		"uncertainDelivery":    store.IsUncertainDelivery,
		"dialmxReady":          dialMXReady,
		"dialmxStatusClass":    dialMXStatusClass,
		"dialmxStatusLabel":    dialMXStatusLabel,
		"dialmxExpiry":         statusExpiry,
		"dnsLight":             dnsLight,
		"dkimStatus":           dkimStatusText,
		"mxStateLabel":         mxStateLabel,
		"feedbackEmail":        func() string { return feedbackEmail },
		"feedbackRepo":         func() string { return feedbackRepo },
		"folderLabel":          folderLabel,
		"roleConventionalName": roleConventionalName,
	}
}

// roleConventionalName returns the conventional folder name for a special folder
// role, used by the connection-settings page's create control.
func roleConventionalName(role string) string {
	if names, ok := roleFolderNames[role]; ok && len(names) > 0 {
		return names[0]
	}
	return role
}

// folderLabel names the current mailbox folder for the "select all N" banner
// ("Inbox", "Sent", "Spam", "Trash", a label's name, or "Drafts").
func folderLabel(folder, label string) string {
	switch folder {
	case "inbox":
		return "Inbox"
	case "sent":
		return "Sent"
	case "spam":
		return "Spam"
	case "trash":
		return "Trash"
	case "drafts":
		return "Drafts"
	case "label":
		if label != "" {
			return label
		}
		return "this label"
	}
	return "this folder"
}

type pageData struct {
	Title string
	Tab   string
	// SettingsTab selects the active settings-modal tab ("personal", "account"
	// or "admin"). It drives both the tab rail and which panel is rendered.
	SettingsTab          string
	Principal            model.Principal
	CSRF                 string
	Account              model.Account
	User                 model.User
	Domains              []model.Domain
	DomainSendingReady   map[string]bool
	DomainReceivingReady map[string]bool
	DomainIsMX           map[string]bool
	InboxSendingReady    map[string]bool
	// InboxReceivingReady maps an inbox id to its receive readiness: a domain
	// inbox's receiving provider, or a standalone inbox's configured IMAP
	// connector. It is distinct from DomainReceivingReady, which has no entry for
	// a standalone inbox (empty DomainID).
	InboxReceivingReady map[string]bool
	// InboxConn maps a provider-connected inbox id to its connection health, for
	// the dashboard's flags column and the Identity tab's reconnect prompt. An
	// inbox with no provider binding (a domain inbox, or an IMAP-only standalone
	// inbox) has no entry.
	InboxConn map[string]inboxConnectionState
	// InboxConnDetail, InboxConnLabel and InboxConnReconnect drive the inbox
	// page's own reconnect banner: a revoked/expired provider connection is a
	// per-inbox problem, so the mailbox view surfaces it in place rather than
	// only on the dashboard. They mirror the matching InboxConn entry for the
	// inbox on screen; an empty detail means no banner renders, and Label is the
	// exact text of the reconnect control.
	InboxConnDetail    string
	InboxConnLabel     string
	InboxConnReconnect string
	// ShowOneReconnect includes the shared in-place One reconnect dialog in the
	// page shell. Only pages that can surface a reconnect control (the dashboard,
	// a mailbox, a folder, a message) set it, so the One setup and import screens,
	// which have their own key entry, never carry a second api_key field.
	ShowOneReconnect bool
	// GoogleClientID and GoogleReconnect prefill the Google wizard when it was
	// opened to reconnect an existing Gmail inbox: the stored client ID is typed
	// back in, and the client-secret entry becomes optional because the stored
	// secret is reused when it is left blank.
	GoogleClientID  string
	GoogleReconnect bool
	// StandaloneInboxes lists the account's standalone inboxes, for the key-dialog
	// role matrix (they have no managed domain to appear under).
	StandaloneInboxes      []model.Inbox
	Domain                 *model.Domain
	DomainInboxes          map[string][]model.Inbox
	DomainSendingEditors   map[string][]*domainEditorView
	DomainReceivingEditors map[string][]*domainEditorView
	// DomainSendingMX marks domains that offer a Direct MX sending editor, so
	// the sending dialog can take the wider layout its DKIM record needs.
	DomainSendingMX map[string]bool
	// DomainSendingLight maps a Direct MX signing domain id to its published
	// DKIM record light (ok/grey/danger) and tooltip, for the Domains table's
	// Sending column. The value is a pointer so a lookup for a non-signing domain
	// yields nil, which templates treat as absent — no light renders.
	DomainSendingLight map[string]*domainLight
	// DomainSendingDKIM marks domains whose Direct MX sending config is signing
	// (has a stored key), so the dialog offers a key rotation.
	DomainSendingDKIM map[string]bool
	// DomainSendingDKIMRevealURL is the reveal endpoint for the DKIM private key,
	// keyed by domain id. It is carried as a map rather than read off the editor
	// so the one-time reveal form can be rendered as a sibling of the sending
	// form (a nested form would be dropped by the HTML parser).
	DomainSendingDKIMRevealURL map[string]string
	DomainSendingSelected      map[string]string
	DomainReceivingSelected    map[string]string
	DomainSendingLabel         map[string]string
	DomainReceivingLabel       map[string]string
	DomainReceivingRegenerate  map[string]bool
	// DialMXSetup is keyed by domain id. The value is a pointer so a lookup for
	// an unconfigured domain yields nil, which templates treat as absent.
	DialMXSetup map[string]*dialMXSetupView
	// MXSetup holds the installation Direct MX receiver status for the per-domain
	// Direct MX dialog. It is shared by every mx-effective domain.
	MXSetup *mxSetupView
	// RemoteMXSetup holds the account's Remote MX receiver panel for the
	// per-domain Remote MX dialog and the account-admin receiver editor. It is
	// shown to account admins only and carries the URL, live state and a
	// redacted key-present flag, never the bearer secret.
	RemoteMXSetup *remoteMXSetupView
	// DomainParentCandidate maps a domain id to the name of the nearest existing
	// ancestor domain, for a domain that was added before its parent and is not
	// linked yet. It lets the sending/receiving provider menus offer "Inherited
	// (from <parent>)", which links the domain on save.
	DomainParentCandidate map[string]string
	DomainOpenID          string
	DomainOpenKind        string
	// DomainWorkerSetup drives the dedicated one-time Cloudflare Worker setup
	// page, not the dashboard.
	DomainWorkerSetup *domainWorkerSetupView
	// DomainCredential is a freshly generated receiving credential (for example
	// Postmark's Basic-auth username/password) shown once after a save or
	// regeneration. Unlike the Cloudflare Worker code it is a short secret with
	// a ready-to-paste webhook URL.
	DomainCredentialTitle        string
	DomainCredentialInstructions string
	DomainCredentialWebhookURL   string
	DomainCredentialSecret       string
	// DKIMPrivateKey is set only on the response to a reveal action, so the
	// sending dialog can show the private key once in a pop-up.
	DKIMPrivateKey string
	// DomainNamesCSV lists the account's domain names (comma separated) so the
	// Add Domain dialog can detect a typed subdomain client-side and offer to
	// reuse the parent's connectors. Server-side detection is authoritative.
	DomainNamesCSV             string
	DomainSendingSettingsURL   string
	DomainReceivingSettingsURL string
	// Sending-paused banner: when the default sender's domain has no connector,
	// point the operator at the domain sending editor.
	SendingPausedExternal bool
	SendingPausedAddress  string
	SendingPausedURL      string
	// InboxOpenID, when set, is the inbox whose edit dialog the dashboard should
	// reopen on load. InboxOpenTab selects the section.
	InboxOpenID    string
	InboxOpenTab   string
	InboxOpenAlias string
	Inboxes        []model.Inbox
	Messages       []model.Message
	// Delivery attempts without an available message are audit events, not mail.
	UnlinkedDeliveries []store.DomainLogEntry
	Credentials        []credentialView
	InboxConnectors    map[string][]credentialView
	PendingRelaySetups []store.PendingRelaySetup
	// AccessGrants maps an inbox id to the secret-free Clients & Access payload
	// (API keys, mailbox users, pending invites) embedded on its edit button.
	AccessGrants map[string]string
	// AuthoringSettingsJSON maps an inbox id to its authoring settings payload,
	// embedded on its edit button so the Approvals tab can load and save them.
	AuthoringSettingsJSON map[string]string
	// InboxRemoteConfig maps a standalone inbox id to its secret-free remote
	// connector view (remoteConfigResponse JSON), embedded on its edit button so
	// the Remote IMAP / Outbound SMTP tabs can populate from durable state.
	InboxRemoteConfig map[string]string
	// AccessMembersJSON lists the account's non-admin members (id + email) so the
	// Clients & Access tab can offer "add existing user" choices client-side.
	AccessMembersJSON string
	// Passkeys lists the signed-in user's registered passkeys, and
	// PasskeyEnabled reports whether the deployment has WebAuthn configured.
	// A deployment without it renders no passkey controls at all.
	Passkeys       []model.WebAuthnCredential
	PasskeyEnabled bool
	LogEntries     []store.DomainLogEntry
	LogHasMore     bool
	LogBefore      string
	// Client delivery log (Webhook / Hermes relay): the client being viewed, its
	// newest entries, and the keyset cursor for the next page.
	ClientLogClient             *credentialView
	ClientLogEntries            []store.ClientDeliveryEntry
	ClientLogHasMore            bool
	ClientLogBefore             int64
	Message                     *model.Message
	MessageHasRemoteImages      bool
	Attachments                 []model.Attachment
	Notice, SecretLabel, Secret string
	Error                       string
	// Page marks which live-update profile a rendered page belongs to
	// ("dashboard" or "inbox"), emitted as a body data attribute so the shared
	// static app.js can subscribe to the right event stream and reconcile the
	// right snapshot. Empty disables live updates on that page.
	Page string
	// Next is a validated same-origin path carried through a login form so a
	// session-expiry redirect can resume the interrupted request.
	Next     string
	HasUsers bool
	BaseURL  string

	Inbox     *model.Inbox
	InboxAddr map[string]string
	// CatchAllInboxes marks the inbox ids that are a domain's enabled catch-all,
	// so lists can show the catch-all marker.
	CatchAllInboxes map[string]bool
	// DomainCatchAllInbox maps a domain id to its enabled catch-all inbox id (if
	// any), so the key matrix can mark and offer the domain-wide sending control
	// on the right inbox row.
	DomainCatchAllInbox map[string]string
	Unread              map[string]int
	MailboxSizes        map[string]int64
	// InboxQuotas maps an inbox id to its storage cap in bytes. A nil pointer
	// means the inbox has no cap of its own (only the account quota applies).
	InboxQuotas map[string]*int64
	UnreadCount int
	SpamCount   int
	TrashCount  int
	DraftCount  int
	OutboxCount int
	HasMore     bool
	Before      string
	// CurrentPageBefore is the incoming pagination boundary of the page being
	// displayed: the `before` value the browser requested with, empty on the
	// newest page. Before is the OUTGOING cursor used to load the next, older
	// page. The two must never be conflated: a refresh must preserve the current
	// page (use CurrentPageBefore), never advance to the next one (Before).
	CurrentPageBefore string
	Folder            string
	PagerURL          string
	// TotalCount is the exact number of items in the current folder/label, used
	// by the "select all N" banner. It is independent of the page size, so a
	// paginated folder can offer a bulk action over every matching item.
	TotalCount    int
	Labels        []string
	UnifiedLabels []mailLabel
	MessageLabels []mailLabel
	LabelUnread   map[string]int
	ActiveLabel   string
	// Capabilities is the inbox's declared capability surface, so the UI hides
	// controls the provider cannot honour.
	Capabilities model.Capabilities
	// Folders is the inbox's physical custom folders, for the common folder
	// sidebar. ActiveFolderID marks the currently-open custom folder.
	Folders        []folderSidebarItem
	ActiveFolderID string
	// CanManageFolders reports that the inbox owns physical folders the
	// operator may create, rename and delete.
	CanManageFolders bool
	// MoveFolders lists the destinations the reader's move control offers.
	MoveFolders []folderSidebarItem
	// StandaloneMode reports that the rendered inbox is a standalone (remote)
	// mailbox, so the mailbox page shows the remote-connection banner and folder
	// management UI. StandaloneConfigured reports whether its connector is set up.
	StandaloneMode       bool
	StandaloneConfigured bool
	StandalonePlain      bool
	// StandaloneProvider marks a provider-connected standalone inbox (One,
	// Google, Microsoft) rather than a hand-configured IMAP/SMTP one, so the
	// mailbox page never tells it to set up an IMAP connector it cannot use.
	StandaloneProvider bool
	// StandaloneAdmin reports that the signed-in principal may repair a provider
	// connection; a mailbox user without it gets the detail but no control.
	StandaloneAdmin    bool
	RemoteConnectorURL string
	OneSetupID         string
	OneChoices         []app.OneMailboxChoice
	OneMailboxes       []oneMailboxStatus
	OneImportResults   []app.OneImportResult
	OneHistoryWindows  []app.OneHistoryWindow
	RemoteConfig       remoteConfigResponse
	// RemoteFolders is the standalone inbox's full folder tree, for the
	// connection-settings role selectors (map an existing folder to a role).
	RemoteFolders []folderListResponse
	// RoleMappings is the connection-settings special-folder mapping table.
	RoleMappings   []roleMappingView
	ThreadMessages []model.Message
	OutboundReady  bool
	InboundReady   bool
	// RemoteBody selects the reader's body block: a remote message's body is
	// fetched live by the browser from /ui/messages/{id}/body (RemoteBodyURL)
	// rather than rendered inline, so opening a standalone message never blocks
	// on the IMAP server.
	RemoteBody    bool
	RemoteBodyURL string
	RemoteHTMLURL string
	RemoteReadURL string

	ComposeTitle, ComposeAction, ComposeCancel string
	ComposeTo, ComposeCC, ComposeBCC           string
	ComposeSubject, ComposeText, ComposeNote   string
	ComposeError, ComposeFlash                 string
	ComposeDraftID                             string
	ComposeFrom                                string
	ComposeFromOptions                         []fromOption

	Drafts []model.Draft

	// Admin plane and account management: the account mailer selection, the
	// invite list, the one-time setup link shown immediately after creation,
	// and the account's mailbox operators.
	AccountMailerInboxID string
	Invites              []inviteView
	InviteLink           string
	Operators            []memberView
	Accounts             []store.AccountSummary
	DraftCounts          map[string]int
	SendRequests         []SendRequestRow
	ReviewDraft          *model.Draft
	// ReviewInvalidReply is the newest invalid approval reply for the pending
	// request shown on the review page, if any.
	ReviewInvalidReply *store.ControlMessage

	TrashRetentionDays int

	// Timezone settings on the account page: the account default and the
	// signed-in user's override, plus the selectable zone names.
	AccountTimezone      string
	AccountTimezoneLabel string
	UserTimezone         string
	TimezoneOptions      []string

	// Key-session Account page: the API key's name/prefix and the mailboxes it
	// can reach, so the page states exactly what the session is.
	KeyName      string
	KeyPrefix    string
	KeyMailboxes []keyMailboxView

	Email string

	// SetupAccountName and SetupError carry the first-run setup form's retained
	// values back across the Post/Redirect/Get round trip.
	SetupAccountName string
}

// keyMailboxView is one row of the key-session Account page's mailbox table.
type keyMailboxView struct {
	Address string
	Role    string
}

// fromOption is one choice in the compose/reply From select: the submitted
// address and a display label that includes the sender name when set.
type fromOption struct {
	Address string
	Label   string
}

type credentialView struct {
	ID, Kind, Name, Type, Scope, RolesJSON, InboxID, Role string
	LastConnectedAt                                       *time.Time
	URL, Mode, AuthMode                                   string
	// DomainSendJSON is a JSON array of inbox ids (a domain's catch-all) that
	// hold the key's live domain-wide sending grant, for the edit dialog.
	DomainSendJSON string
	Admin          bool
	Enabled        bool
}

// connectorsJSON returns the secret-free connector metadata embedded on an
// inbox settings button. html/template escapes it for attribute context.
func connectorsJSON(connectors []credentialView) string {
	if connectors == nil {
		connectors = []credentialView{}
	}
	b, err := json.Marshal(connectors)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// inboxesJSON returns the {address,name} pairs of a domain's mailboxes for the
// domain delete dialog. html/template escapes it for attribute context.
func inboxesJSON(boxes []model.Inbox) string {
	out := make([]map[string]string, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, map[string]string{"address": b.Address, "name": b.DisplayName})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// accessKeyView is one API key holding (or implicitly holding) a role on a
// single inbox, as embedded in the Clients & Access tab. Admin keys have no
// per-inbox binding and are rendered read-only.
type accessKeyView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	Admin bool   `json:"admin,omitempty"`
	// DomainSend reports whether this key holds the domain-wide sending grant on
	// this inbox, and whether that grant is currently live (the inbox is still
	// the domain's enabled catch-all).
	DomainSend bool `json:"domain_send,omitempty"`
}

// accessUserView is one human login with a role on a single inbox. Humans are
// always Owner today; the Role field keeps the shape ready for a later ladder.
type accessUserView struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role,omitempty"`
}

// accessInviteView is a pending operator invitation that includes this inbox.
type accessInviteView struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// accessGrantView is the per-inbox "who can access this inbox" payload embedded
// as JSON on the inbox settings button. It is secret-free.
type accessGrantView struct {
	Keys   []accessKeyView    `json:"keys"`
	Admin  []accessKeyView    `json:"admin_keys"`
	Users  []accessUserView   `json:"users"`
	Invite []accessInviteView `json:"invites"`
	// CatchAll reports that this inbox is its domain's current catch-all, so the
	// Clients & Access panel offers the domain-wide sending control. It is false
	// for every other inbox, where the control is meaningless.
	CatchAll bool `json:"catch_all,omitempty"`
}

// accessGrantJSON marshals an inbox's access grant to secret-free JSON for the
// edit button's data attribute. html/template escapes it in attribute context.
func accessGrantJSON(g accessGrantView) string {
	if g.Keys == nil {
		g.Keys = []accessKeyView{}
	}
	if g.Admin == nil {
		g.Admin = []accessKeyView{}
	}
	if g.Users == nil {
		g.Users = []accessUserView{}
	}
	if g.Invite == nil {
		g.Invite = []accessInviteView{}
	}
	b, err := json.Marshal(g)
	if err != nil {
		return `{"keys":[],"admin_keys":[],"users":[],"invites":[]}`
	}
	return string(b)
}

// accessGrantsByInbox builds the per-inbox Clients & Access payload from the
// account's API keys and human users, plus its pending operator invitations.
// Only admins reach this code path, so the caller has already scoped the data.
// catchAll maps each domain's catch-all inbox id to its domain id, so an inbox
// that is its domain's catch-all is marked and offered the domain-wide sending
// control.
func accessGrantsByInbox(boxes []model.Inbox, keys []model.APIKey, users []model.User, invites []model.Invite, catchAll map[string]string, now time.Time) map[string]string {
	grants := make(map[string]accessGrantView, len(boxes))
	for _, b := range boxes {
		// The control is offered only where the grant could be live: the inbox is
		// its domain's current catch-all and is enabled. A disabled catch-all is
		// the same as no catch-all for sending, so the control is hidden there.
		_, isCatchAll := catchAll[b.ID]
		grants[b.ID] = accessGrantView{CatchAll: isCatchAll && b.Enabled}
	}
	for _, k := range keys {
		if k.Admin {
			for id, g := range grants {
				g.Admin = append(g.Admin, accessKeyView{ID: k.ID, Name: k.Name, Admin: true})
				grants[id] = g
			}
			continue
		}
		for inboxID, role := range k.Roles {
			g, ok := grants[inboxID]
			if !ok {
				continue
			}
			g.Keys = append(g.Keys, accessKeyView{ID: k.ID, Name: k.Name, Role: role, DomainSend: k.DomainSend[inboxID]})
			grants[inboxID] = g
		}
	}
	for _, u := range users {
		if u.IsAdmin || u.SystemAdmin {
			continue
		}
		for inboxID, role := range u.Roles {
			g, ok := grants[inboxID]
			if !ok {
				continue
			}
			g.Users = append(g.Users, accessUserView{ID: u.ID, Email: u.Email, Role: role})
			grants[inboxID] = g
		}
	}
	for _, inv := range invites {
		if inv.Kind != model.InviteKindOperator || !inv.Pending(now) {
			continue
		}
		for _, inboxID := range inv.InboxIDs {
			g, ok := grants[inboxID]
			if !ok {
				continue
			}
			g.Invite = append(g.Invite, accessInviteView{ID: inv.ID, Email: inv.Email})
			grants[inboxID] = g
		}
	}
	out := make(map[string]string, len(grants))
	for id, g := range grants {
		sort.Slice(g.Keys, func(i, j int) bool { return g.Keys[i].Name < g.Keys[j].Name })
		sort.Slice(g.Admin, func(i, j int) bool { return g.Admin[i].Name < g.Admin[j].Name })
		sort.Slice(g.Users, func(i, j int) bool { return g.Users[i].Email < g.Users[j].Email })
		sort.Slice(g.Invite, func(i, j int) bool { return g.Invite[i].Email < g.Invite[j].Email })
		out[id] = accessGrantJSON(g)
	}
	return out
}

// accessMembersJSON marshals the account's non-admin members to JSON for the
// Clients & Access tab's "add existing user" chooser.
func accessMembersJSON(users []model.User) string {
	type member struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	out := []member{}
	for _, u := range users {
		if u.IsAdmin || u.SystemAdmin {
			continue
		}
		out = append(out, member{ID: u.ID, Email: u.Email})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// secretFlash carries a one-time secret from the POST that created it to the
// dashboard GET that displays it, so refreshing cannot create it again.
type secretFlash struct {
	Notice, Label, Secret string
}

func inboxAddrMap(boxes []model.Inbox) map[string]string {
	m := make(map[string]string, len(boxes))
	for _, b := range boxes {
		m[b.ID] = b.Address
	}
	return m
}

// inboxReadiness derives the per-domain sending/receiving readiness and the
// effective per-inbox sending readiness rendered by the inbox tables. A send
// uses the chosen (or default) sender's connector: a managed alias on another
// domain is ready only when that domain has a sending config.
func inboxReadiness(domains []model.Domain, boxes []model.Inbox) (sendingReady, receivingReady, inboxSendingReady, inboxReceivingReady map[string]bool) {
	sendingReady = make(map[string]bool, len(domains))
	receivingReady = make(map[string]bool, len(domains))
	domainInboxesByName := make(map[string]string, len(domains))
	for _, d := range domains {
		domainInboxesByName[strings.ToLower(d.Name)] = d.ID
		sendingReady[d.ID] = d.SendingProvider != ""
		receivingReady[d.ID] = d.ReceivingProvider != ""
	}
	inboxSendingReady = make(map[string]bool, len(boxes))
	inboxReceivingReady = make(map[string]bool, len(boxes))
	for _, b := range boxes {
		if b.Kind == model.InboxKindStandalone {
			// A standalone inbox's readiness is its IMAP/SMTP connector, not a
			// managed domain: sending needs a bound SMTP server; receiving needs
			// a configured remote connector. An empty DomainID must not read as
			// "domain not configured".
			inboxSendingReady[b.ID] = b.Remote != nil && b.Remote.SMTP != nil
			if b.Capabilities != nil && b.Capabilities.Outbound {
				inboxSendingReady[b.ID] = true
			}
			inboxReceivingReady[b.ID] = b.RemoteConfigured && b.Remote != nil
			continue
		}
		effective := b.DomainID
		if name := domainNameOf(b.DefaultSender); name != "" {
			if id, ok := domainInboxesByName[name]; ok {
				effective = id
			}
		}
		inboxSendingReady[b.ID] = sendingReady[effective]
		inboxReceivingReady[b.ID] = receivingReady[effective]
	}
	return sendingReady, receivingReady, inboxSendingReady, inboxReceivingReady
}

// isOutboundDelivery reports whether a log row is an outbound delivery
// transition, the kinds the dashboard collapses.
func isOutboundDelivery(kind string) bool {
	switch kind {
	case "sent", "failed", "sending", "interrupted":
		return true
	}
	return false
}

// deliveryKey identifies the delivery a log row belongs to. Rows that share a
// key are successive state transitions of one send, not separate sends.
func deliveryKey(entry store.DomainLogEntry) string {
	if entry.DeliveryKey != "" {
		return entry.DeliveryKey
	}
	return "row:" + entry.ID
}

// latestDeliveries returns the newest transition of each outbound delivery.
// The delivery log records every state transition of a send, so one message
// contributes several rows; the newest is the message's current delivery
// state.
func latestDeliveries(entries []store.DomainLogEntry) map[string]store.DomainLogEntry {
	latest := map[string]store.DomainLogEntry{}
	for _, entry := range entries {
		if !isOutboundDelivery(entry.Kind) {
			continue
		}
		key := deliveryKey(entry)
		cur, ok := latest[key]
		if !ok || entry.At.After(cur.At) || (entry.At.Equal(cur.At) && entry.Attempt > cur.Attempt) {
			latest[key] = entry
		}
	}
	return latest
}

// unlinkedDeliveryEvents returns the durable outbound transitions without a
// message record. Do not infer an identity from subject/recipient/timing:
// two independently sent messages can have identical visible fields.
func unlinkedDeliveryEvents(entries []store.DomainLogEntry) []store.DomainLogEntry {
	out := []store.DomainLogEntry{}
	for _, entry := range entries {
		if isOutboundDelivery(entry.Kind) && entry.MessageID == "" {
			out = append(out, entry)
		}
	}
	return out
}

// dashboardActivityRows renders the account activity log as dashboard rows.
// The outbound log records every state transition of a send, so one message
// contributes several rows: they are collapsed to the latest transition, so
// the dashboard shows a message's current delivery state rather than a history
// of its transitions.
func dashboardActivityRows(entries []store.DomainLogEntry) []model.Message {
	latest := latestDeliveries(entries)
	msgs := make([]model.Message, 0, len(entries))
	for _, entry := range entries {
		if isOutboundDelivery(entry.Kind) && entry.MessageID == "" {
			// Delivery-only history belongs to its own audit table, not
			// the Recent messages table with an unopenable duplicate row.
			continue
		}
		direction := "inbound"
		status := ""
		attempts := 0
		if isOutboundDelivery(entry.Kind) {
			win, ok := latest[deliveryKey(entry)]
			if !ok || win.ID != entry.ID {
				// A superseded transition: this delivery is already on the
				// list under its latest state.
				continue
			}
			direction = "outbound"
			status = entry.Kind
			attempts = entry.Attempt
		}
		msgs = append(msgs, model.Message{
			ID:        entry.MessageID,
			InboxID:   entry.InboxID,
			Direction: direction,
			From:      model.Address{Address: entry.FromAddress},
			To:        entry.To,
			Subject:   entry.Subject,
			Source:    entry.Source,
			SizeBytes: entry.SizeBytes,
			CreatedAt: entry.At,
			Status:    status,
			Attempts:  attempts,
			Blocked:   entry.Kind == "blocked",
			Approval:  entry.Kind == "approval",
		})
	}
	return msgs
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	has, err := s.Service.Store.HasUsers(r.Context())
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if !has {
		if wantsHTML(r) {
			http.Redirect(w, r, "/setup", 303)
			return
		}
		s.discovery(w, r)
		return
	}
	c, err := r.Cookie("mmm_session")
	if err != nil {
		s.serveDiscoveryOrLogin(w, r)
		return
	}
	p, cval, err := s.Service.Store.SessionPrincipal(r.Context(), c.Value)
	if err != nil {
		s.serveDiscoveryOrLogin(w, r)
		return
	}
	ctx := context.WithValue(r.Context(), principalKey, p)
	ctx = context.WithValue(ctx, csrfKey, cval)
	ctx = withTimezone(ctx, p)
	r = r.WithContext(ctx)
	if p.Admin {
		s.dashboard(w, r)
		return
	}
	s.operatorDashboard(w, r)
}

// serveDiscoveryOrLogin keeps browsers on the login flow while letting a
// session-less API client bootstrap from the base URL: a request that does not
// ask for HTML receives the discovery document instead of a redirect.
func (s *Server) serveDiscoveryOrLogin(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		http.Redirect(w, r, "/login", 303)
		return
	}
	s.discovery(w, r)
}

// wantsHTML reports whether the client prefers an HTML page, so the root URL
// can serve the human UI to browsers and the discovery document to agents.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

const authBody = `<style>.auth-form{margin:0}.auth-form label{display:block}.auth-form input{margin:3px 0 8px}.auth-action{margin-top:8px}.auth-button{display:flex;width:100%;height:40px;box-sizing:border-box;align-items:center;justify-content:center;text-align:center;line-height:1.2;padding:0 12px}.auth-action p{margin:4px 0 0}</style><div class="card" style="max-width:460px;margin:60px auto"><h1>{{.Title}}</h1>{{if .Notice}}<div class="error">{{.Notice}}</div>{{end}}<form method="post" class="auth-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="next" value="{{.Next}}"><label>Email</label><input type="email" name="email" required value="{{.Email}}"><label>Password</label><input type="password" name="password" minlength="10" required><button class="auth-button">{{.Title}}</button></form>{{if .PasskeyEnabled}}<div class="auth-action"><button type="button" class="secondary auth-button" id="passkey-signin" data-begin="/login/webauthn/begin" data-finish="/login/webauthn/finish">Sign in with a passkey</button><p class="muted small" id="passkey-status" role="status" aria-live="polite"></p></div>{{end}}<div class="auth-action"><a class="btn secondary auth-button" href="/login/key">Sign in with an API key</a></div><div style="margin-top:12px;padding-top:10px;border-top:1px solid #eee"><p style="font-size:14px;margin:0 0 6px">Agents: see <a href="/agent">/agent</a> for API access instructions</p><p class="muted" style="font-size:12px;margin:0">Reference: <a href="/openapi.json">/openapi.json</a> · <a href="/examples/python">/examples/python</a> · <a href="/examples/bash">/examples/bash</a> · <a href="/.well-known/mail-api">/.well-known/mail-api</a></p></div></div>`

const keyLoginBody = `<style>.auth-form{margin:0}.auth-form label{display:block}.auth-form input{margin:3px 0 8px}.auth-button{display:flex;width:100%;height:40px;box-sizing:border-box;align-items:center;justify-content:center;text-align:center;line-height:1.2;padding:0 12px}.auth-actions{display:grid;gap:8px;margin-top:8px}</style><div class="card" style="max-width:460px;margin:60px auto"><h1>Sign in with an API key</h1>{{if .Notice}}<div class="error" role="alert">{{.Notice}}</div>{{end}}<form method="post" action="/login/key" class="auth-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>API key</label><input type="password" name="api_key" autocomplete="off" spellcheck="false" placeholder="mmm_…" required autofocus><div class="auth-actions"><button class="auth-button">Sign in</button><a class="btn secondary auth-button" href="/login">Cancel</a></div></form></div>`

// setupBody is the first-run setup form shown while the database has no users.
// The first visitor claims the instance as its system administrator; the POST
// is a one-shot, atomic claim in the store and the route self-disables once any
// user exists. An operator may alternatively pre-create the system
// administrator from ADMIN_EMAIL / ADMIN_PASSWORD before startup, in which case
// this page is never shown.
const setupBody = `<style>.auth-form{margin:0}.auth-form label{display:block}.auth-form input{margin:3px 0 8px}.auth-button{display:flex;width:100%;height:40px;box-sizing:border-box;align-items:center;justify-content:center;text-align:center;line-height:1.2;padding:0 12px}</style><div class="card" style="max-width:460px;margin:60px auto"><h1>Set up Mammoth Mail</h1><p class="muted">This instance has no administrator yet. Create one to claim it; this page is then permanently disabled.</p>{{if .Notice}}<div class="error" role="alert">{{.Notice}}</div>{{end}}<form method="post" action="/setup" class="auth-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Account name</label><input name="account" maxlength="80" value="{{.SetupAccountName}}" placeholder="Optional, defaults to your email name"><label>Email</label><input type="email" name="email" required value="{{.Email}}" autocomplete="username"><label>Password</label><input type="password" name="password" minlength="10" required autocomplete="new-password"><label>Confirm password</label><input type="password" name="confirm" minlength="10" required autocomplete="new-password"><button class="auth-button">Create administrator</button></form><div style="margin-top:12px;padding-top:10px;border-top:1px solid #eee"><p class="muted" style="font-size:12px;margin:0">Operators can instead set <code>ADMIN_EMAIL</code> and <code>ADMIN_PASSWORD</code> before startup.</p></div></div>`

// feedbackEmail and feedbackRepo are the two destinations the in-app Feedback
// panel points users at. They are intentionally constants beside the shell that
// renders the button, so changing one is a one-line edit.
const feedbackEmail = "mammothmail@hgolabs.com"

const feedbackRepo = "https://github.com/dellarb/mammothmail"

type authFlash struct {
	Title, Error, Email string
}

// renderAuth shows an auth page, restoring any error and email left by a
// redirect from a failed POST (Post/Redirect/Get).
func (s *Server) renderAuth(w http.ResponseWriter, r *http.Request, title string) {
	data := pageData{Title: title, CSRF: s.setPreAuthCSRF(w, r), PasskeyEnabled: s.webauthn != nil}
	if next := safeNextPath(r.URL.Query().Get("next")); next != "/" {
		data.Next = next
		// A resumed request always came from an expired session; tell the user.
		data.Notice = "Your session expired. Please sign in to continue."
	}
	if v, ok := s.peekFlash(r, r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(authFlash); ok {
			s.takeFlash(r, r.URL.Query().Get("_flash"))
			data.Title, data.Notice, data.Email = f.Title, f.Error, f.Email
		}
	}
	// Carry only an opaque token through login. Draft contents cannot be read
	// until a newly authenticated request resolves to the original owner.
	if tok := r.URL.Query().Get("_flash"); tok != "" && data.Next != "" {
		u, _ := url.Parse(data.Next)
		q := u.Query()
		if q.Get("_flash") == "" {
			q.Set("_flash", tok)
		}
		u.RawQuery = q.Encode()
		data.Next = u.String()
	}
	body := strings.Replace(authBody, `href="/login/key"`, `href="/login/key?next={{.Next | querystring}}"`, 1)
	s.render(w, r, body, data)
}

// flashAuth stores an auth error and redirects back to the form.
func (s *Server) flashAuth(w http.ResponseWriter, r *http.Request, dest, title, msg, email string) {
	query := url.Values{}
	if next := safeNextPath(r.Form.Get("next")); next != "/" {
		query.Set("next", next)
	}
	msg = safeErrorMessage(errors.New(msg), "Something went wrong. Please try again.")
	if tok := s.putFlash(w, r, authFlash{Title: title, Error: msg, Email: email}, len(title)+len(msg)+len(email)+32); tok != "" {
		query.Set("_flash", tok)
	}
	if len(query) > 0 {
		dest += "?" + query.Encode()
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// setupGet shows the first-run setup form on an unconfigured install, letting
// the first visitor claim the instance as its system administrator. Once any
// user exists the page redirects to the login page; the matching POST is the
// one-shot claim and is atomic in the store. An operator can still pre-create
// the system administrator from ADMIN_EMAIL / ADMIN_PASSWORD, in which case the
// instance is already configured and this page is never shown.
func (s *Server) setupGet(w http.ResponseWriter, r *http.Request) {
	has, err := s.Service.Store.HasUsers(r.Context())
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if has {
		http.Redirect(w, r, "/login", 303)
		return
	}
	data := pageData{Title: "Set up Mammoth Mail", CSRF: s.setPreAuthCSRF(w, r)}
	if v, ok := s.takeFlash(r, r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(setupFlash); ok {
			data.Notice, data.Email, data.SetupAccountName = f.Error, f.Email, f.AccountName
		}
	}
	s.render(w, r, setupBody, data)
}

// setupFlash restores the form fields and any error after a failed setup POST
// (Post/Redirect/Get), so the first visitor does not re-enter everything.
type setupFlash struct {
	Error, Email, AccountName string
}

// setupPost claims an unconfigured instance: it creates the first account and
// its system-administrator user in one atomic store operation, then signs the
// new administrator in. The route is only reachable while no user exists; a
// second claim (concurrent or replayed) gets ErrConflict and is sent to the
// login page instead of overwriting the administrator.
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	has, err := s.Service.Store.HasUsers(r.Context())
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if has {
		http.Redirect(w, r, "/login", 303)
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	ip := clientIP(r, s.Service.Config)
	if s.setupLimiter != nil && !s.setupLimiter.Allow(rateLimitKey(ip)) {
		http.Error(w, "too many setup attempts", 429)
		return
	}
	_ = r.ParseForm()
	email := strings.TrimSpace(r.Form.Get("email"))
	password := r.Form.Get("password")
	accountName := strings.TrimSpace(r.Form.Get("account"))
	if password != r.Form.Get("confirm") {
		s.flashSetup(w, r, "Passwords do not match", email, accountName)
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		s.flashSetup(w, r, safeErrorMessage(err, "Could not complete setup. Please try again."), email, accountName)
		return
	}
	if accountName == "" {
		accountName = strings.Split(email, "@")[0]
	}
	u, err := s.Service.Store.CreateInitialAdmin(r.Context(), accountName, email, password, s.Service.Config.DefaultQuotaBytes)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Another request claimed the instance first. The administrator now
			// exists; a fresh page render would redirect, so send them to login.
			s.flashAuth(w, r, "/login", "Log In", "This instance has already been set up. Sign in instead.", "")
			return
		}
		s.flashSetup(w, r, safeErrorMessage(err, "Could not complete setup. Please try again."), email, accountName)
		return
	}
	tok, _, err := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	if err != nil {
		http.Error(w, "session error", 500)
		return
	}
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/", 303)
}

// flashSetup stores a setup error and redirects back to the form, preserving
// the non-secret values the visitor already typed.
func (s *Server) flashSetup(w http.ResponseWriter, r *http.Request, msg, email, accountName string) {
	msg = safeErrorMessage(errors.New(msg), "Could not complete setup. Please try again.")
	dest := "/setup"
	if tok := s.putFlash(w, r, setupFlash{Error: msg, Email: email, AccountName: accountName}, len(msg)+len(email)+len(accountName)+32); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
func (s *Server) registerGet(w http.ResponseWriter, r *http.Request) {
	if !s.Service.Config.AllowRegistration {
		http.Error(w, "registration is closed", 403)
		return
	}
	s.renderAuth(w, r, "Create Account")
}
func (s *Server) registerPost(w http.ResponseWriter, r *http.Request) {
	if !s.Service.Config.AllowRegistration {
		http.Error(w, "registration is closed", 403)
		return
	}
	// Bound signup abuse: a public deployment cannot let a single source
	// create accounts without limit.
	ip := clientIP(r, s.Service.Config)
	if s.registerLimiter != nil && !s.registerLimiter.Allow(rateLimitKey(ip)) {
		http.Error(w, "too many registration attempts", 429)
		return
	}
	_ = r.ParseForm()
	name := r.Form.Get("account")
	if name == "" {
		name = strings.Split(r.Form.Get("email"), "@")[0]
	}
	u, err := s.Service.Store.CreateAccountAndAdmin(r.Context(), name, r.Form.Get("email"), r.Form.Get("password"), s.Service.Config.DefaultQuotaBytes)
	if err != nil {
		s.flashAuth(w, r, "/register", "Create Account", safeErrorMessage(err, "Could not create account. Please try again."), r.Form.Get("email"))
		return
	}
	tok, _, _ := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/", 303)
}
func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	s.renderAuth(w, r, "Log In")
}

func (s *Server) keyLoginGet(w http.ResponseWriter, r *http.Request) {
	data := pageData{Title: "Sign in with an API key", CSRF: s.setPreAuthCSRF(w, r)}
	data.Next = safeNextPath(r.URL.Query().Get("next"))
	if v, ok := s.takeFlash(r, r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(authFlash); ok {
			data.Notice = f.Error
		}
	}
	body := strings.Replace(keyLoginBody, `<label>API key</label>`, `<input type="hidden" name="next" value="{{.Next}}"><label>API key</label>`, 1)
	s.render(w, r, body, data)
}
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.Service.Config)
	if !s.loginLimiter.Allow(rateLimitKey(ip)) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	_ = r.ParseForm()
	email := strings.TrimSpace(r.Form.Get("email"))
	// Per-account throttle, independent of source address, so rotating IPs
	// cannot spray one account behind the per-IP limiter.
	accountKey := strings.ToLower(email)
	if accountKey != "" && s.accountLoginLimiter != nil && !s.accountLoginLimiter.Allow(accountKey) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	u, err := s.Service.Store.AuthenticateUserWithin(r.Context(), email, r.Form.Get("password"), argonAdmissionTimeout)
	if err != nil {
		if errors.Is(err, store.ErrAuthBusy) {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "server busy, try again", http.StatusServiceUnavailable)
			return
		}
		s.flashAuth(w, r, "/login", "Log In", "Invalid email or password", email)
		return
	}
	if accountKey != "" && s.accountLoginLimiter != nil {
		s.accountLoginLimiter.Reset(accountKey)
	}
	tok, _, err := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	if err != nil {
		http.Error(w, "session error", 500)
		return
	}
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, safeNextPath(r.Form.Get("next")), 303)
}

// safeNextPath returns a same-origin next path for a post-login redirect, or "/"
// when none is valid.
func safeNextPath(next string) string {
	return safePath(next, "/")
}

// safePath returns a same-origin root-relative redirect path, or fallback when
// none is valid. It rejects scheme-relative and backslash forms that browsers
// may normalize to an off-site redirect, and any control character: browsers
// strip ASCII tab/newline from a URL, so "/\t/evil.com" would resolve to
// "//evil.com" off-site. Failing closed on anything that does not parse as a
// scheme-less, host-less, root-relative path covers both.
func safePath(next, fallback string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return fallback
	}
	for _, r := range next {
		if r < 0x20 || r == 0x7f {
			return fallback
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return fallback
	}
	return next
}

// keyLoginPost signs in a browser session from a non-admin mailbox API key.
// Only keys that carry at least one mailbox binding are accepted; the session
// that results mirrors the key's own scope and never carries the account Admin
// or system administrator role. An admin key (which has no mailbox bindings) is
// rejected: this path maps mailbox access, not admin mode. A revoked key is
// refused by the principal lookup.
func (s *Server) keyLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.Service.Config)
	if s.keyLoginLimiter == nil || !s.keyLoginLimiter.Allow(rateLimitKey(ip)) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	_ = r.ParseForm()
	key := strings.TrimSpace(r.Form.Get("api_key"))
	if key == "" {
		s.flashAuth(w, r, "/login/key", "Sign in with an API key", "Enter an API key", "")
		return
	}
	p, err := s.Service.Store.APIKeyPrincipal(r.Context(), key)
	if err != nil || p.Admin || len(p.MailboxRoles) == 0 {
		// One generic message for unknown, revoked, admin and binding-less keys,
		// so the form never reveals which class a supplied key belongs to.
		s.flashAuth(w, r, "/login/key", "Sign in with an API key", "That API key cannot be used to sign in", "")
		return
	}
	tok, _, err := s.Service.Store.CreateKeySession(r.Context(), p.APIKeyID, s.keySessionTTL())
	if err != nil {
		http.Error(w, "session error", 500)
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "key.login", p.APIKeyID)
	s.setKeySessionCookie(w, r, tok)
	http.Redirect(w, r, safeNextPath(r.Form.Get("next")), 303)
}
func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("mmm_session"); err == nil {
		s.Service.Store.DeleteSession(r.Context(), c.Value)
		if p := principal(r); p.SessionHash != "" {
			s.Service.Hub.CancelScope("sess:" + p.SessionHash)
		}
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", 303)
}

// settingsFlash carries an error or success notice from a settings POST to the
// settings GET (Post/Redirect/Get), so a refresh cannot re-submit the form.
type settingsFlash struct {
	Error, Notice string
}

// settingsShellOpen and settingsShellClose wrap the active settings tab in a
// full-screen modal dialog, shared by the account page and the system-admin
// plane. The tab rail links to real URLs (/account and /admin), so each tab is
// separately addressable, bookmarkable and refreshable, and the modal
// auto-opens on load; tabs the principal may not use are omitted entirely.
const settingsShellOpen = `<dialog id="settings-dialog" class="inbox-settings" aria-labelledby="settings-title">
<div class="inbox-settings-head"><h2 id="settings-title">Settings</h2><button type="button" class="secondary icon-btn settings-close" aria-label="Close"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></div>
<div class="inbox-settings-body"><nav class="inbox-settings-nav" role="tablist" aria-label="Settings sections">
<a class="dialog-tab{{if eq .SettingsTab "personal"}} active{{end}}" role="tab" aria-selected="{{if eq .SettingsTab "personal"}}true{{else}}false{{end}}" href="/account">Personal</a>
{{if .Principal.Admin}}<a class="dialog-tab{{if eq .SettingsTab "account"}} active{{end}}" role="tab" aria-selected="{{if eq .SettingsTab "account"}}true{{else}}false{{end}}" href="/account?tab=account">Account</a>{{end}}
{{if .Principal.SystemAdmin}}<a class="dialog-tab{{if eq .SettingsTab "admin"}} active{{end}}" role="tab" aria-selected="{{if eq .SettingsTab "admin"}}true{{else}}false{{end}}" href="/admin">Admin</a>{{end}}
</nav><div class="inbox-settings-panels">`

const settingsShellClose = `</div></div><footer class="inbox-settings-footer"><span class="spacer"></span><button type="button" class="secondary settings-close">Cancel</button>{{if or (eq .SettingsTab "personal") (eq .SettingsTab "account")}}<button type="submit" form="settings-{{.SettingsTab}}-form">Save</button>{{end}}</footer></dialog>`

const settingsBody = settingsShellOpen + `{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if eq .SettingsTab "account"}}
<form id="settings-account-form" method="post" action="/ui/account/settings">
<input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="_tab" value="account">
<section class="dialog-panel" role="tabpanel">
<h3 class="section-head">Account settings</h3>
<p class="muted">These settings are for this account. The time zone is the default for operators; Trash retention is the default for inboxes.</p>
<div class="setting-row"><div class="setting-label"><label for="settings-account-name">Account name</label><p class="muted small">Shown in the header; a display name, not an email address.</p></div><input id="settings-account-name" name="name" value="{{.Account.Name}}" maxlength="80" required></div>
<div class="setting-row"><div class="setting-label"><label for="settings-account-tz">Account time zone</label><p class="muted small">Default for operators who have not set their own. Leave blank for UTC.</p></div><input id="settings-account-tz" name="timezone" list="tz-options" value="{{.AccountTimezone}}" placeholder="{{.AccountTimezoneLabel}}" autocomplete="off"></div>
<div class="setting-row"><div class="setting-label"><label for="settings-account-trash">Trash auto-purge (days)</label><p class="muted small">Trashed messages are purged after this many days; 0 keeps them until emptied manually.</p></div><input id="settings-account-trash" name="days" type="number" min="0" max="3650" value="{{.TrashRetentionDays}}" required></div>
</section>
</form>
{{end}}
{{if eq .SettingsTab "personal"}}
<form id="settings-personal-form" method="post" action="/ui/account/settings/me">
<input type="hidden" name="_csrf" value="{{.CSRF}}">
<section class="dialog-panel" role="tabpanel">
<h3 class="section-head">Your settings</h3>
<p class="muted">These apply to you only, not to the rest of the account.</p>
<div class="setting-row"><div class="setting-label"><label for="settings-user-tz">Your time zone</label><p class="muted small">Leave blank to follow the account default.</p></div><input id="settings-user-tz" name="timezone" list="tz-options" value="{{.UserTimezone}}" placeholder="Account default ({{.AccountTimezoneLabel}})" autocomplete="off"></div>
{{if not .User.SystemAdmin}}
<div class="setting-row"><div class="setting-label"><label for="settings-user-email">Email address</label><p class="muted small">Used to log in. Current: {{.User.Email}}</p></div><input id="settings-user-email" type="email" name="email" value="{{.User.Email}}" autocomplete="username"></div>
<div class="setting-row"><div class="setting-label"><label for="settings-user-current">Current password</label><p class="muted small">Required to change your email or password.</p></div><input id="settings-user-current" type="password" name="current_password" autocomplete="current-password"></div>
<div class="setting-row"><div class="setting-label"><label for="settings-user-new">New password</label><p class="muted small">Leave blank to keep your current password.</p></div><input id="settings-user-new" type="password" name="new_password" minlength="10" autocomplete="new-password"></div>
<div class="setting-row"><div class="setting-label"><label for="settings-user-confirm">Confirm new password</label></div><input id="settings-user-confirm" type="password" name="confirm_password" minlength="10" autocomplete="new-password"></div>
{{else}}
<div class="setting-row"><div class="setting-label"><label>Login credentials</label><p class="muted small">Your password is managed by the deployment configuration. Update <code>ADMIN_EMAIL</code> and <code>ADMIN_PASSWORD</code> (or their <code>_FILE</code> secrets) and restart Mammoth Mail; the new password takes effect and other sessions are signed out. This password always works as a recovery method. You may also add passkeys below as an additional, independent way to sign in.</p></div></div>
{{end}}
</section>
</form>
{{if .PasskeyEnabled}}<h3 class="section-head">Passkeys</h3>
<p class="muted">Sign in without a password using a passkey (Touch ID, Windows Hello, or a security key). Add one per device.{{if not .User.SystemAdmin}} When you add a passkey you can choose to make it your only sign-in method.{{end}}{{if not .User.PasswordEnabled}} <b>Password sign-in is currently disabled.</b>{{end}}</p>{{if .Passkeys}}<ul class="slist passkey-list">{{range .Passkeys}}<li><span class="alias-row-name">{{.Name}}</span><button type="button" class="secondary icon-btn open-passkey-settings" data-passkey="{{.ID}}" title="Passkey settings" aria-label="Settings for passkey {{.Name}}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0 .33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></button></li>{{end}}</ul>{{else}}<p class="muted">No passkeys yet.</p>{{end}}<div class="dialog-actions">{{if and (not .User.PasswordEnabled) (not .User.SystemAdmin)}}<form method="post" action="/ui/account/passkeys/password" data-confirm="Re-enable password sign-in?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button type="button" class="secondary">Re-enable password sign-in</button></form>{{end}}<button type="button" class="secondary" id="passkey-add" data-begin="/ui/account/passkeys/begin" data-finish="/ui/account/passkeys/finish" data-... (line truncated to 2000 chars)password-enabled="{{if and .User.PasswordEnabled (not .User.SystemAdmin)}}1{{else}}0{{end}}">Add a passkey</button></div><p class="muted small" id="passkey-status" role="status" aria-live="polite"></p>{{range .Passkeys}}<dialog id="passkey-dialog-{{.ID}}" class="passkey-dialog"><h2>Passkey settings</h2><dl class="dialog-usage"><dt>Name</dt><dd>{{.Name}}</dd><dt>Added</dt><dd>{{mailDate .CreatedAt}}</dd><dt>Last used</dt><dd>{{lastUsedDate .LastUsedAt}}</dd><dt>Backups</dt><dd>{{passkeyBackup .}}</dd></dl><form method="post" action="/ui/account/passkeys/rename"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><label>Name</label><input name="name" value="{{.Name}}" maxlength="80" required><button class="secondary">Rename</button></form><div class="dialog-actions"><div class="dialog-danger"><form method="post" action="/ui/account/passkeys/delete" data-confirm="Remove this passkey? You will no longer be able to sign in with it."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><button class="secondary danger">Remove passkey</button></form></div><button type="button" class="secondary" data-close-dialog>Cancel</button></div></dialog>{{end}}
{{end}}
{{end}}
{{if eq .SettingsTab "account"}}{{template "account-operators" .}}{{end}}
<datalist id="tz-options">{{range .TimezoneOptions}}<option value="{{.}}"></option>{{end}}</datalist>
` + settingsShellClose

// settingsRedirect stores a settings flash and redirects back to the settings
// page (Post/Redirect/Get).
func (s *Server) settingsRedirect(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	errMsg = safeErrorMessage(errors.New(errMsg), "Could not save settings. Please try again.")
	dest := "/account"
	if r.Form.Get("_tab") == "account" {
		dest = "/account?tab=account"
	}
	if tok := s.putFlash(w, r, settingsFlash{Error: errMsg, Notice: notice}, len(notice)+len(errMsg)+32); tok != "" {
		if r.Form.Get("_tab") == "account" {
			dest += "&_flash=" + tok
		} else {
			dest += "?_flash=" + tok
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.APIKeyID != "" && p.UserID == "" {
		s.keySessionSettings(w, r, p)
		return
	}
	acc, err := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	if err != nil {
		http.Error(w, "account not found", 404)
		return
	}
	user, err := s.Service.Store.GetUser(r.Context(), p.UserID)
	if err != nil {
		http.Error(w, "user not found", 404)
		return
	}
	accountTZLabel := acc.Timezone
	if accountTZLabel == "" {
		accountTZLabel = timezone.Default
	}
	settingsTab := "personal"
	if r.URL.Query().Get("tab") == "account" && p.Admin {
		settingsTab = "account"
	}
	data := pageData{Title: "Account", Tab: "account", SettingsTab: settingsTab, Principal: p, CSRF: csrf(r), Account: acc, User: user,
		AccountTimezone: acc.Timezone, AccountTimezoneLabel: accountTZLabel, UserTimezone: user.Timezone, TimezoneOptions: timezone.Options()}
	data.PasskeyEnabled = s.webauthn != nil
	if creds, cerr := s.Service.Store.WebAuthnCredentialsForUser(r.Context(), p.UserID); cerr == nil {
		data.Passkeys = creds
	}
	if p.Admin {
		if days, derr := s.Service.Store.GetTrashRetention(r.Context(), p); derr == nil {
			data.TrashRetentionDays = days
		}
	}
	// The account page can receive two flash kinds: a settings result and a
	// one-time invitation link. Dispatch on the stored type.
	if tok := r.URL.Query().Get("_flash"); tok != "" {
		if v, ok := s.peekFlash(r, tok); ok {
			switch v.(type) {
			case settingsFlash:
				if taken, ok := s.takeFlash(r, tok); ok {
					if tf, ok := taken.(settingsFlash); ok {
						data.Error, data.Notice = tf.Error, tf.Notice
					}
				}
			case inviteFlash:
				if taken, ok := s.takeFlash(r, tok); ok {
					if tf, ok := taken.(inviteFlash); ok {
						data.InviteLink = tf.Link
					}
				}
			}
		}
	}
	if p.Admin {
		ctx := r.Context()
		members, err := s.Service.Store.ListAccountUsers(ctx, p.AccountID)
		if err != nil {
			http.Error(w, "cannot list operators", 500)
			return
		}
		inboxes, _ := s.Service.Store.ListInboxes(ctx, p)
		addresses := make(map[string]string, len(inboxes))
		for _, b := range inboxes {
			addresses[b.ID] = b.Address
		}
		data.Operators = operatorViews(members, addresses)
		data.Inboxes = inboxes
		mailer, _ := s.Service.Store.AccountMailerInboxID(ctx, p.AccountID)
		data.AccountMailerInboxID = mailer
		invites, _ := s.Service.Store.ListInvites(ctx, p.AccountID)
		now := time.Now().UTC()
		for _, inv := range invites {
			if inv.Kind != model.InviteKindOperator || !inv.Pending(now) {
				continue
			}
			data.Invites = append(data.Invites, newInviteView(inv, now))
		}
	}
	s.render(w, r, settingsBody, data)
}

// keySessionBody is the Account page shown to a browser session derived from an
// API key. It is deliberately small: a key is not a person, so there are no
// email, password, passkey or personal time-zone controls, and no account
// administration. It states only what the session is and what it can reach.
const keySessionBody = `<h1>Signed in with an API key</h1>
<p class="muted">This browser session uses the API key <b>{{.KeyName}}</b> ({{.KeyPrefix}}…). It has exactly the mailbox access of that key and nothing more. Closing the session ends it; the key itself is unaffected.</p>
<div class="grid">
<section class="card"><h2>Mailbox access</h2>{{if .KeyMailboxes}}<div class="table-wrap"><table class="dense"><thead><tr><th>Mailbox</th><th>Role</th></tr></thead><tbody>{{range .KeyMailboxes}}<tr><td>{{.Address}}</td><td>{{.Role}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">This key has no mailbox access.</p>{{end}}</section>
<section class="card"><h2>End session</h2><p class="muted">Sign out of this browser session. Your API key continues to work for API clients.</p><form method="post" action="/logout"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button>Sign out</button></form></section>
</div>`

// keySessionSettings renders the slim Account page for a key-derived session.
func (s *Server) keySessionSettings(w http.ResponseWriter, r *http.Request, p model.Principal) {
	acc, err := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	if err != nil {
		http.Error(w, "account not found", 404)
		return
	}
	boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
	addresses := make(map[string]string, len(boxes))
	for _, b := range boxes {
		addresses[b.ID] = b.Address
	}
	rows := make([]keyMailboxView, 0, len(p.MailboxRoles))
	for inboxID, role := range p.MailboxRoles {
		addr := addresses[inboxID]
		if addr == "" {
			continue
		}
		rows = append(rows, keyMailboxView{Address: addr, Role: role})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Address < rows[j].Address })
	name, prefix, _ := s.Service.Store.APIKeyNamePrefix(r.Context(), p.APIKeyID)
	data := pageData{Title: "Account", Tab: "account", Principal: p, CSRF: csrf(r), Account: acc,
		KeyName: name, KeyPrefix: prefix, KeyMailboxes: rows}
	s.render(w, r, keySessionBody, data)
}

func (s *Server) uiSettingsAccount(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		s.settingsRedirect(w, r, "", "You do not have permission to change this setting")
		return
	}
	if err := s.Service.Store.UpdateAccountName(r.Context(), p.AccountID, r.Form.Get("name")); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.rename", "")
	s.settingsRedirect(w, r, "Account name updated", "")
}

// uiSettingsTrashRetention updates the account's Trash auto-purge window. It
// requires an account Admin (see apiAccountSettings).
func (s *Server) uiSettingsTrashRetention(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.CanAdministerAccount() {
		s.settingsRedirect(w, r, "", "You do not have permission to change this setting")
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("days")))
	if err != nil || days < 0 {
		s.settingsRedirect(w, r, "", "Retention must be a whole number of days (0 or more)")
		return
	}
	if err := s.Service.Store.SetTrashRetention(r.Context(), p, days); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.trash_retention", strconv.Itoa(days))
	s.settingsRedirect(w, r, "Trash retention updated", "")
}

// uiSettingsAccountTimezone sets the account default display time zone. It
// requires an account Admin. An empty value means UTC.
func (s *Server) uiSettingsAccountTimezone(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		s.settingsRedirect(w, r, "", "You do not have permission to change this setting")
		return
	}
	tz := strings.TrimSpace(r.Form.Get("timezone"))
	if err := s.Service.Store.SetAccountTimezone(r.Context(), p, tz); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.timezone", tz)
	s.settingsRedirect(w, r, "Account time zone updated", "")
}

// uiSettingsUserTimezone sets the signed-in user's display time zone override.
// An empty value clears the override so the account default applies.
func (s *Server) uiSettingsUserTimezone(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	tz := strings.TrimSpace(r.Form.Get("timezone"))
	if err := s.Service.Store.SetUserTimezone(r.Context(), p, tz); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.timezone", tz)
	s.settingsRedirect(w, r, "Time zone updated", "")
}

func (s *Server) uiSettingsEmail(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.SystemAdmin {
		s.settingsRedirect(w, r, "", "The system administrator's login is managed by the deployment configuration (ADMIN_EMAIL / ADMIN_PASSWORD).")
		return
	}
	ip := clientIP(r, s.Service.Config)
	if !s.passwordLimiter.Allow(rateLimitKey(ip)) {
		s.settingsRedirect(w, r, "", "too many attempts, try again later")
		return
	}
	err := s.Service.Store.UpdateUserEmail(r.Context(), p.UserID, p.AccountID, r.Form.Get("email"), r.Form.Get("current_password"))
	if err != nil {
		msg := safeErrorMessage(err, "Could not update sign-in details. Please try again.")
		switch {
		case errors.Is(err, store.ErrForbidden):
			msg = "Current password is incorrect"
		case errors.Is(err, store.ErrConflict):
			msg = "That email address is already in use"
		}
		s.settingsRedirect(w, r, "", msg)
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.email_change", "")
	s.settingsRedirect(w, r, "Email address updated", "")
}

func (s *Server) uiSettingsPassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.SystemAdmin {
		s.settingsRedirect(w, r, "", "The system administrator's login is managed by the deployment configuration (ADMIN_EMAIL / ADMIN_PASSWORD).")
		return
	}
	ip := clientIP(r, s.Service.Config)
	if !s.passwordLimiter.Allow(rateLimitKey(ip)) {
		s.settingsRedirect(w, r, "", "too many attempts, try again later")
		return
	}
	current := r.Form.Get("current_password")
	newPassword := r.Form.Get("new_password")
	if newPassword != r.Form.Get("confirm_password") {
		s.settingsRedirect(w, r, "", "New passwords do not match")
		return
	}
	if newPassword == current {
		s.settingsRedirect(w, r, "", "New password must be different from the current one")
		return
	}
	keep := ""
	if c, err := r.Cookie("mmm_session"); err == nil {
		keep = c.Value
	}
	if err := s.Service.Store.UpdateUserPassword(r.Context(), p.UserID, current, newPassword, keep); err != nil {
		msg := safeErrorMessage(err, "Could not update sign-in details. Please try again.")
		if errors.Is(err, store.ErrForbidden) {
			msg = "Current password is incorrect"
		}
		s.settingsRedirect(w, r, "", msg)
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.password_change", "")
	// Other devices were logged out in the database; cancel their live streams.
	s.Service.Hub.CancelScope("user:" + p.UserID)
	s.settingsRedirect(w, r, "Password changed. Other devices have been logged out.", "")
}

// uiSettingsAccountBatch applies the Account tab's rows (name, default time
// zone and Trash retention) in one save. It requires an account Admin; every
// row is account-wide state, so a mailbox Owner is not sufficient.
func (s *Server) uiSettingsAccountBatch(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		s.settingsRedirect(w, r, "", "You do not have permission to change this setting")
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" {
		s.settingsRedirect(w, r, "", "Account name is required")
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("days")))
	if err != nil || days < 0 {
		s.settingsRedirect(w, r, "", "Retention must be a whole number of days (0 or more)")
		return
	}
	tz := strings.TrimSpace(r.Form.Get("timezone"))
	if err := s.Service.Store.UpdateAccountName(r.Context(), p.AccountID, name); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.rename", "")
	if err := s.Service.Store.SetAccountTimezone(r.Context(), p, tz); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.timezone", tz)
	if err := s.Service.Store.SetTrashRetention(r.Context(), p, days); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.trash_retention", strconv.Itoa(days))
	s.settingsRedirect(w, r, "Account settings updated", "")
}

// uiSettingsPersonalBatch applies the Personal tab's rows (display time zone,
// and optionally the sign-in email and password) in one save. Email and password
// changes are applied only when filled in, and each requires the current
// password, verified by the store.
func (s *Server) uiSettingsPersonalBatch(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	tz := strings.TrimSpace(r.Form.Get("timezone"))
	email := strings.TrimSpace(r.Form.Get("email"))
	newPassword := r.Form.Get("new_password")
	confirm := r.Form.Get("confirm_password")
	current := r.Form.Get("current_password")

	changingEmail := false
	if !p.SystemAdmin && email != "" {
		if u, err := s.Service.Store.GetUser(r.Context(), p.UserID); err == nil && !strings.EqualFold(u.Email, email) {
			changingEmail = true
		}
	}
	changingPassword := newPassword != "" || confirm != ""
	if changingEmail || changingPassword {
		ip := clientIP(r, s.Service.Config)
		if !s.passwordLimiter.Allow(rateLimitKey(ip)) {
			s.settingsRedirect(w, r, "", "too many attempts, try again later")
			return
		}
	}
	if changingPassword && newPassword != confirm {
		s.settingsRedirect(w, r, "", "New passwords do not match")
		return
	}
	if p.SystemAdmin && changingPassword {
		s.settingsRedirect(w, r, "", "The system administrator's login is managed by the deployment configuration (ADMIN_EMAIL / ADMIN_PASSWORD).")
		return
	}

	if err := s.Service.Store.SetUserTimezone(r.Context(), p, tz); err != nil {
		s.settingsRedirect(w, r, "", safeErrorMessage(err, "Could not save settings. Please try again."))
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.timezone", tz)

	if changingEmail {
		err := s.Service.Store.UpdateUserEmail(r.Context(), p.UserID, p.AccountID, email, current)
		if err != nil {
			msg := safeErrorMessage(err, "Could not update sign-in details. Please try again.")
			switch {
			case errors.Is(err, store.ErrForbidden):
				msg = "Current password is incorrect"
			case errors.Is(err, store.ErrConflict):
				msg = "That email address is already in use"
			}
			s.settingsRedirect(w, r, "", msg)
			return
		}
		s.Service.Store.Audit(r.Context(), p.AccountID, "user.email_change", "")
	}

	if changingPassword {
		if newPassword == current {
			s.settingsRedirect(w, r, "", "New password must be different from the current one")
			return
		}
		keep := ""
		if c, err := r.Cookie("mmm_session"); err == nil {
			keep = c.Value
		}
		if err := s.Service.Store.UpdateUserPassword(r.Context(), p.UserID, current, newPassword, keep); err != nil {
			msg := safeErrorMessage(err, "Could not update sign-in details. Please try again.")
			if errors.Is(err, store.ErrForbidden) {
				msg = "Current password is incorrect"
			}
			s.settingsRedirect(w, r, "", msg)
			return
		}
		s.Service.Store.Audit(r.Context(), p.AccountID, "user.password_change", "")
		s.Service.Hub.CancelScope("user:" + p.UserID)
	}

	s.settingsRedirect(w, r, "Personal settings updated", "")
}

// The key-dialog's OpenClaw finish screen (#key-openclaw-guide) mirrors the
// install steps in docs/CONNECTORS.md, section "OpenClaw". Keep them in step.
const dashboardBaseBody = `{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error notice" role="alert" aria-live="assertive">{{.Error}}</div>{{end}}{{if .Secret}}<div class="secret"><b>{{.SecretLabel}}</b><pre>{{.Secret}}</pre><button type="button" class="secondary" data-copy-value="{{.Secret}}">Copy</button></div>{{end}}
  {{if .DomainCredentialSecret}}<dialog id="domain-credential-dialog" class="domain-dialog" data-open="1"><h2>{{.DomainCredentialTitle}}</h2><p class="muted">{{.DomainCredentialInstructions}}</p><ol class="steps"><li>In Postmark, select your Server → Inbound Message Stream → Settings.</li><li>Set the Inbound Domain to this receiving domain. Enable Include raw email content in JSON payload.</li><li>Paste this authenticated URL into the Webhook field. It includes the generated username and password and is shown only once.<div class="setup-url-row"><pre id="domain-credential-url">{{.DomainCredentialWebhookURL}}</pre><button type="button" class="secondary icon-btn" id="domain-credential-copy" title="Copy webhook URL" aria-label="Copy webhook URL">{{copyIcon}}</button></div></li><li>Save the settings in Postmark. Enable SMTP in the Server settings as required for inbound domain forwarding.</li><li>Publish an MX record for the receiving domain pointing to inbound.postmarkapp.com with priority 10. Changing MX records redirects incoming mail; do not replace an existing mail service unintentionally.</li><li>Send a test email to an inbox on this domain and confirm it arrives in Mammoth Mail.</li></ol><p class="muted small">Treat this URL as a password. If lost, regenerate the secret from Receiving; old credentials stop working immediately.</p><div class="dialog-actions"><button type="button" class="btn" data-close-dialog>Done</button></div></dialog>{{end}}
  {{if .DKIMPrivateKey}}<dialog id="dkim-key-dialog" class="domain-dialog" data-open="1"><h2>DKIM private key</h2><p class="muted">This is the key Mammoth Mail signs mail for this domain with. It is shown only now; keep a copy somewhere safe if you want a backup. Do not publish it.</p><div class="secret"><pre id="dkim-private-key">{{.DKIMPrivateKey}}</pre></div><div class="dialog-actions"><button type="button" class="secondary" id="dkim-key-copy">Copy key</button><button type="button" class="btn" data-close-dialog>Done</button></div></dialog>{{end}}
<div class="tab-panel"{{if ne .Tab "home"}} hidden{{end}}>
<template id="pending-relay-setups">{{range .PendingRelaySetups}}<div class="connector-row" data-inbox="{{.InboxID}}" data-expires="{{.ExpiresAt.Format "2006-01-02T15:04:05Z07:00"}}"><div class="connector-row-main"><div class="connector-row-name">{{.Name}}</div><div class="connector-row-meta">OpenClaw · Pending enrollment · Expires {{localDateTime .ExpiresAt}}</div></div><button type="submit" form="cancel-relay-{{.ID}}" class="secondary btn-sm danger">Cancel setup</button></div>{{end}}</template>
{{range .PendingRelaySetups}}<form hidden id="cancel-relay-{{.ID}}" method="post" action="/ui/openclaw/setup/{{.ID}}/cancel"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="inbox" value="{{.InboxID}}"></form>{{end}}
<div class="grid dashboard-grid"><section class="card" style="grid-column:1/-1" data-open-inbox="{{.InboxOpenID}}" data-open-inbox-tab="{{.InboxOpenTab}}" data-open-alias="{{.InboxOpenAlias}}" data-access-members="{{.AccessMembersJSON}}"><div class="card-head"><h2>Inboxes</h2><button type="button" id="add-inbox">Add inbox</button></div>{{if .Inboxes}}{{template "inboxes-table" .}}{{else}}<p class="muted">No inboxes yet.</p>{{end}}</section>
<section class="card"><div class="card-head"><h2>Clients</h2><button type="button" id="add-key">Add Client</button></div>{{if .Credentials}}<div class="table-wrap"><table class="dense"><thead><tr><th>Name</th><th>Type</th><th></th></tr></thead><tbody>{{range .Credentials}}<tr{{if ne .Kind "api"}} data-connector-row{{end}}><td>{{.Name}}</td><td>{{.Type}}</td><td class="actions">{{if or (eq .Kind "webhook") (eq .Kind "hermes") (eq .Kind "openclaw")}}<a class="btn secondary icon-btn" href="/ui/clients/{{.ID}}/log" title="Delivery log" aria-label="Delivery log"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3.5h10M3 7h10M3 10.5h7"/><path d="M12 14h1M3 14h6"/></svg></a>{{end}}<button type="button" class="secondary icon-btn edit-credential" data-id="{{.ID}}" data-kind="{{.Kind}}" data-name="{{.Name}}" data-admin="{{if .Admin}}1{{end}}" data-roles="{{.RolesJSON}}" data-domainsend="{{.DomainSendJSON}}" data-inbox="{{.InboxID}}" data-role="{{.Role}}" data-url="{{.URL}}" data-mode="{{.Mode}}" data-auth="{{.AuthMode}}" title="Client settings" aria-label="Client settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></button><button type="button" class="secondary icon-btn danger open-delete-client" data-kind="{{if eq .Kind "hermes"}}hermes{{else if eq .Kind "openclaw"}}openclaw{{else if eq .Kind "webhook"}}webhooks{{else}}keys{{end}}" data-id="{{.ID}}" data-name="{{.Name}}" data-type="{{.Type}}" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No clients yet.</p>{{end}}</section>
<section class="card"><div class="card-head"><h2>Domains</h2><button type="button" id="add-domain">Add Domain</button></div>{{if .Domains}}<div class="table-wrap"><table class="domains-table"><thead><tr><th>Domain</th><th>Catch-all</th><th>Sending</th><th>Receiving</th><th></th></tr></thead><tbody data-domain-rows>{{range .Domains}}{{$d := .}}<tr data-domain-id="{{.ID}}"><td><b>{{.Name}}</b>{{if .ParentDomainID}} <span class="subdomain-tag" title="Subdomain of {{.ParentDomain}}"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2.2 4.6 7h1.8L4 10.6h8L9.6 7h1.8L8 2.2Z"/><path d="M8 10.6V14"/><path d="M5.8 14h4.4"/></svg></span>{{end}}</td><td>{{if .CatchAllInboxID}}<button type="button" class="cell-link domain-catchall-link open-domain-dialog" data-domain="{{.ID}}" data-kind="catchall" title="{{index $.InboxAddr .CatchAllInboxID}}">{{index $.InboxAddr .CatchAllInboxID}}</button>{{else}}<button type="button" class="secondary btn-sm cell-edit domain-catchall-add open-domain-dialog" data-domain="{{.ID}}" data-kind="catchall">Add</button>{{end}}</td><td><button type="button" class="{{if .SendingProvider}}secondary{{else}}amber{{end}} btn-sm cell-edit domain-provider-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="sending" data-sending-button>{{with index $.DomainSendingLight .ID}}<span class="dns-light {{.Light}}" data-sending-light title="{{.Title}}" aria-label="{{.Title}}"></span> {{end}}{{if .SendingProvider}}{{if .SendingInheritedFrom}}<span class="inherited">(inherited)</span>{{else}}{{index $.DomainSendingLabel .ID}}{{end}}{{else}}Add{{end}}</button></td><td><button type="button" class="{{if .ReceivingProvider}}secondary{{else}}amber{{end}} btn-sm cell-edit domain-provider-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="receiving" data-receiving-button>{{with index $.DialMXSetup .ID}}{{if .Light}}<span class="dns-light {{.Light}}" data-receiving-light title="{{.LightTitle}}" aria-label="{{.LightTitle}}"></span>{{end}}{{end}}{{if .ReceivingProvider}}{{if .ReceivingInheritedFrom}}<span class="inherited">(inherited)</span>{{else}}{{index $.DomainReceivingLabel .ID}}{{end}}{{else}}Add{{end}}{{if and (eq .ReceivingProvider "mx") $.MXSetup (not $.MXSetup.Configured)}} <span class="pill amber" title="The installation's bundled SMTP receiver is not configured. Incoming mail will not be received.">Not receiving</span>{{end}}</button></td><td><span class="domain-actions"><a class="btn secondary icon-btn" href="/ui/domains/{{.ID}}/sending/deliveries" title="Activity log" aria-label="Activity log"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="2" width="10" height="12" rx="2"/><path d="M5 5.5h6M5 8.5h6M5 11.5h4"/></svg></a><button type="button" class="secondary icon-btn danger open-delete-domain" data-domain="{{.ID}}" data-name="{{.Name}}" data-inboxes="{{inboxesJSON (index $.DomainInboxes .ID)}}" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></span></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">Add your first domain.</p>{{end}}</section></div>
<section class="card"><h2>Recent messages</h2><form method="get" action="/" class="search-form"><input name="q" value="" placeholder="Search mail"><button>Search</button></form>{{if .Messages}}<div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Direction</th><th>From</th><th>To</th><th>Subject</th><th>Source</th><th></th></tr></thead><tbody>{{range .Messages}}<tr><td style="white-space:nowrap">{{localDateTime .CreatedAt}}</td><td>{{if .Blocked}}<span class="pill amber">Blocked</span>{{else if eq .Direction "outbound"}}{{if eq .Status "failed"}}<span class="pill danger">Failed</span>{{else if eq .Status "sending"}}<span class="pill amber">Sending…</span>{{else if eq .Status "interrupted"}}<span class="pill danger">Interrupted</span>{{else if eq .Status "pending"}}<span class="pill amber">Pending</span>{{else}}<span class="pill">Sent</span>{{end}}{{else}}<span class="pill">Received</span>{{end}}{{if gt .Attempts 1}} <span class="muted small">attempt {{.Attempts}}</span>{{end}}</td><td>{{if .From.Address}}{{.From.Address}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Subject}}{{.Subject}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if eq .Source "Control"}}<span class="pill">Control</span>{{else if .Source}}{{.Source}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if or .Blocked .Approval (not .ID)}}<span class="muted">—</span>{{else}}<a href="/ui/messages/{{.ID}}">Open</a>{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No messages yet.</p>{{end}}</section>
{{if .UnlinkedDeliveries}}<section class="card"><h2>Unlinked outbound delivery events</h2><p class="muted small">Delivery state changes without an available message record. Each row is an audit event, not a separate email. These records are preserved for investigation.</p><div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Status</th><th>From</th><th>To</th><th>Subject</th><th>Provider</th></tr></thead><tbody>{{range .UnlinkedDeliveries}}<tr><td style="white-space:nowrap">{{localDateTime .At}}</td><td>{{.Status}}{{if gt .Attempt 0}} <span class="muted small">attempt {{.Attempt}}</span>{{end}}</td><td>{{.FromAddress}}</td><td>{{join .To ", "}}</td><td>{{.Subject}}</td><td>{{.Provider}} <span class="muted small">log #{{.ID}}</span></td></tr>{{end}}</tbody></table></div></section>{{end}}
</div>

<dialog id="key-dialog"><h2 id="key-dialog-title">Add client</h2><form method="post" action="/ui/keys" id="key-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><label>Type</label><select name="type" id="key-type"><optgroup label="Clients" data-group="clients"><option value="api">API key</option></optgroup><optgroup label="Connectors"><option value="hermes">Hermes relay connection</option><option value="openclaw">OpenClaw agent connector</option><option value="webhook">Webhook delivery</option></optgroup></select><p class="banner warn" id="key-openclaw-alpha" hidden><b>Early alpha.</b> The OpenClaw connector is in early access and still being tested, so expect rough edges and changes.</p><label>Name</label><input name="name" placeholder="Hermes EA" required><fieldset class="key-fields" data-type="api" style="border:0;padding:0;margin:0"><label><input type="checkbox" name="admin" value="1"> Account Admin Key (Full permission on all mailboxes and can create and delete mailboxes)</label><fieldset id="key-matrix" style="border:0;padding:0;margin:0">{{if .Inboxes}}<table class="key-matrix"><thead><tr><th>Inbox</th><th><span class="muted">Set all:</span> <div class="seg" data-set-scope="all"><button type="button" data-set-role="">None</button><button type="button" data-set-role="read">Read</button><button type="button" data-set-role="assistant">Assistant</button><button type="button" data-set-role="owner">Owner</button></div></th></tr></thead>{{range .Domains}}{{$d := .}}{{if index $.DomainInboxes $d.ID}}<tbody data-domain="{{$d.ID}}"><tr class="domain-row"><td><b>{{$d.Name}}</b></td><td><div class="seg" data-set-scope="{{$d.ID}}"><button type="button" data-set-role="" data-domain="{{$d.ID}}">None</button><button type="button" data-set-role="read" data-domain="{{$d.ID}}">Read</button><button type="button" data-set-role="assistant" data-domain="{{$d.ID}}">Assistant</button><button type="button" data-set-role="owner" data-domain="{{$d.ID}}">Owner</button></div></td></tr>{{range index $.DomainInboxes $d.ID}}<tr><td class="domain-inbox">{{.Address}}{{if index $.CatchAllInboxes .ID}} {{catchAllIcon}}{{end}}</td><td>{{if eq .ID (index $.DomainCatchAllInbox $d.ID)}}<div class="seg key-matrix-domainsend"><input type="hidden" name="domainsend_{{.ID}}" value="0"><button type="button" class="key-matrix-domainsend-btn" data-inbox="{{.ID}}" aria-pressed="false" title="Send as any address on {{$d.Name}}. Requires an enabled catch-all inbox. This also grants Owner.">Domain send</button></div>{{else}}<div class="seg key-matrix-domainsend-spacer" aria-hidden="true"></div>{{end}} <div class="seg"><input type="radio" id="role_{{.ID}}_none" name="role_{{.ID}}" value="" checked><label for="role_{{.ID}}_none">None</label><input type="radio" id="role_{{.ID}}_read" name="role_{{.ID}}" value="read"><label for="role_{{.ID}}_read">Read</label><input type="radio" id="role_{{.ID}}_assistant" name="role_{{.ID}}" value="assistant"><label for="role_{{.ID}}_assistant">Assistant</label><input type="radio" id="role_{{.ID}}_owner" name="role_{{.ID}}" value="owner"><label for="role_{{.ID}}_owner">Owner</label></div></td></tr>{{end}}</tbody>{{end}}{{end}}{{if $.StandaloneInboxes}}<tbody data-standalone="1"><tr class="domain-row"><td><b>Standalone inboxes</b></td><td><div class="seg" data-set-scope="standalone"><button type="button" data-set-role="" data-domain="standalone">None</button><button type="button" data-set-role="read" data-domain="standalone">Read</button><button type="button" data-set-role="assistant" data-domain="standalone">Assistant</button><button type="button" data-set-role="owner" data-domain="standalone">Owner</button></div></td></tr>{{range $.StandaloneInboxes}}<tr><td class="domain-inbox">{{.Address}}</td><td><div class="seg key-matrix-domainsend-spacer" aria-hidden="true"></div> <div class="seg"><input type="radio" id="role_{{.ID}}_none" name="role_{{.ID}}" value="" checked><label for="role_{{.ID}}_none">None</label><input type="radio" id="role_{{.ID}}_read" name="role_{{.ID}}" value="read"><label for="role_{{.ID}}_read">Read</label><input type="radio" id="role_{{.ID}}_assistant" name="role_{{.ID}}" value="assistant"><label for="role_{{.ID}}_assistant">Assistant</label><input type="radio" id="role_{{.ID}}_owner" name="role_{{.ID}}" value="owner"><label for="role_{{.ID}}_owner">Owner</label></div></td></tr>{{end}}</tbody>{{end}}</table>{{else}}<p class="muted">Create an inbox first to grant mailbox access.</p>{{end}}<table class="role-legend"><thead><tr><th>Role</th><th>Grants</th></tr></thead><tbody><tr><td>Read</td><td>Read messages/threads, search, download attachments.</td></tr><tr><td>Assistant</td><td>Read plus delete messages and create/edit drafts. Cannot send.</td></tr><tr><td>Owner</td><td>Full mailbox access: read, delete, send.</td></tr></tbody></table></fieldset></fieldset><fieldset class="key-fields" data-type="hermes" style="border:0;padding:0;margin:0"><label class="connector-inbox-label">Inbox</label><select class="connector-inbox-select" name="inbox">{{range .Inboxes}}<option value="{{.ID}}" data-allowlist="{{if .SenderRestricted}}1{{end}}">{{.Address}}</option>{{end}}</select><label>Outbound authority</label><select name="role"><option value="owner">Owner — relay sends directly</option><option value="assistant">Assistant — relay drafts and requests approval</option></select><div class="banner" id="key-hermes-warning" hidden style="background:#fdecef;border-color:#e0a0aa;color:#b00020"><b>This inbox has no allow list.</b> The Hermes agent will respond to anyone who emails this inbox. We strongly recommend you set an allow list of permitted senders before creating a Hermes relay connection to this mailbox. Click edit next to the mailbox to configure an allow list.</div><label id="key-hermes-ack-row" hidden style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="ack" value="1" id="key-hermes-ack" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>I understand the risk of my agent responding to anyone who emails it</span></label><div class="connector-auto-actions"><h3 class="section-head" style="margin-top:12px">After a connector handles mail</h3><p class="muted small">Automatic actions once this connector receives a message.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="auto_mark_read_on_delivery" value="1" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Mark messages read once delivered</span></label><label style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="auto_trash_after_delivery" value="1" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Move messages to Trash after delivery</span></label><div><label>Trash after (hours)</label><input type="number" name="auto_trash_after_delivery_hours" value="24" min="1" max="87600"></div><label>Trigger when</label><select name="delivery_trigger"><option value="any">Any connector has delivered</option><option value="all" selected>All connectors present at receipt have delivered</option></select></div></fieldset><fieldset class="key-fields" data-type="openclaw" style="border:0;padding:0;margin:0"><label class="connector-inbox-label">Inbox</label><select class="connector-inbox-select" name="inbox">{{range .Inboxes}}<option value="{{.ID}}" data-allowlist="{{if .SenderRestricted}}1{{end}}">{{.Address}}</option>{{end}}</select><label>Outbound authority</label><select name="role"><option value="owner">Owner — agent sends directly</option><option value="assistant">Assistant — agent drafts and requests approval</option></select><label>Setup method</label><select name="setup"><option value="code">One-time code — run openclaw channels add (recommended)</option><option value="manual">Manual config block — for air-gapped installs</option></select><div class="banner" id="key-openclaw-warning" hidden style="background:#fdecef;border-color:#e0a0aa;color:#b00020"><b>This inbox has no allow list.</b> Your OpenClaw agent will respond to anyone who emails this inbox. We strongly recommend you set an allow list of permitted senders before creating an OpenClaw connector to this mailbox. Click edit next to the mailbox to configure an allow list.</div><label id="key-openclaw-ack-row" hidden style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="ack" value="1" id="key-openclaw-ack" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>I understand the risk of my agent responding to anyone who emails it</span></label><div class="connector-auto-actions"><h3 class="section-head" style="margin-top:12px">After a connector handles mail</h3><p class="muted small">Automatic actions once this connector receives a message.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="auto_mark_read_on_delivery" value="1" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Mark messages read once delivered</span></label><label style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="auto_trash_after_delivery" value="1" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Move messages to Trash after delivery</span></label><div><label>Trash after (hours)</label><input type="number" name="auto_trash_after_delivery_hours" value="24" min="1" max="87600"></div><label>Trigger when</label><select name="delivery_trigger"><option value="any">Any connector has delivered</option><option value="all" selected>All connectors present at receipt have delivered</option></select></div></fieldset><fieldset class="key-fields" data-type="webhook" style="border:0;padding:0;margin:0"><label class="connector-inbox-label">Inbox</label><select class="connector-inbox-select" name="inbox">{{range .Inboxes}}<option value="{{.ID}}">{{.Address}}</option>{{end}}</select><label>Destination URL</label><input name="url" type="url" placeholder="https://example.com/hook" autocomplete="off"><label>Payload</label><select name="mode"><option value="notify">Notify — small JSON with the message id</option><option value="forward">Forward — full raw MIME</option></select><label>Authentication</label><select name="auth"><option value="signature">Signature — signed HMAC-SHA256 header</option><option value="bearer">Bearer — static token</option></select><div class="connector-auto-actions"><h3 class="section-head" style="margin-top:12px">After a connector handles mail</h3><p class="muted small">Automatic actions once this connector receives a message.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="auto_mark_read_on_delivery" value="1" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Mark messages read once delivered</span></label><label style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="auto_trash_after_delivery" value="1" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Move messages to Trash after delivery</span></label><div><label>Trash after (hours)</label><input type="number" name="auto_trash_after_delivery_hours" value="24" min="1" max="87600"></div><label>Trigger when</label><select name="delivery_trigger"><option value="any">Any connector has delivered</option><option value="all" selected>All connectors present at receipt have delivered</option></select></div></fieldset><div class="error" id="key-error" hidden></div><div class="dialog-actions"><button type="button" class="amber" id="key-rotate" hidden>Rotate Key</button><button type="button" class="secondary" id="key-cancel">Cancel</button><button id="key-submit">Add Client</button></div></form><div id="key-result" hidden><h3 id="key-result-title"></h3><ol class="steps" id="key-openclaw-guide" hidden><li>On the OpenClaw host (Node 24.16 or later), install the Mammoth Mail plugin. It is not on npm or ClawHub yet, so build it from the Mammoth Mail repository:<pre>cd plugins/openclaw/openclaw-plugin
npm ci &amp;&amp; npm run build
npm pack --pack-destination /tmp
openclaw plugins install npm-pack:/tmp/mammothmail-openclaw-0.1.0.tgz --accept-capabilities</pre></li><li>Then add the connector using the details below. OpenClaw's “Learned” notice needs OpenClaw-side configuration; the Hermes settings below do not fix it.</li></ol><ol class="steps" id="key-hermes-guide" hidden>
<li><b>Copy the generated block</b> into your gateway's <code>.env</code> — usually <code>~/.hermes/.env</code>. Replace any existing <code>GATEWAY_RELAY_*</code> lines; only one relay connection can own a gateway.<div class="secret" style="margin-top:12px"><pre id="key-hermes-secret"></pre></div><div class="banner warn" style="margin-left:24px;margin-top:12px"><b>Optional but recommended — reduce extra emails</b><ol type="a"><li>Set a home address alongside the relay lines in the same <code>.env</code>, so Hermes does not send correspondents a “No home channel is set” notice.<pre>EMAIL_HOME_ADDRESS=you@yourdomain.com
EMAIL_HOME_ADDRESS_NAME=Your Agent Name</pre></li>
<li>Turn off memory and self-improvement notices globally in <code>~/.hermes/config.yaml</code> (merge into any existing <code>display</code> section):<pre>display:
  memory_notifications: off
  platforms:
    email:
      suppress_warning_notifications: true</pre></li></ol></div></li>
<li><b>Restart the Hermes gateway (required):</b> run <code>hermes gateway restart</code> to load the configuration changes.</li>
</ol><p class="muted" id="key-result-label"></p><div class="secret"><pre id="key-result-secret"></pre></div><div class="dialog-actions"><button type="button" class="secondary" id="key-copy">Copy</button><button type="button" id="key-done">Done</button></div></div></dialog>
<dialog id="add-domain-dialog"><form method="post" action="/ui/domains" data-domain-names="{{.DomainNamesCSV}}"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Domain</label><input name="name" id="add-domain-name" placeholder="example.com" required aria-describedby="add-domain-hint"><p class="muted small" id="add-domain-hint">Enter the bare domain (<code>example.com</code>). Add the whole domain even if you only use a few addresses. Configure sending and receiving from the domain's row in the Domains list.</p><div id="add-domain-inherit" hidden><p class="muted small"><b><span class="add-domain-parent"></span></b> is already configured. Reuse its connectors for this subdomain, or untick to configure it separately.</p><label class="inherit-option"><input type="checkbox" name="inherit_receiving" value="1" checked> <span>Use <b class="add-domain-parent"></b>&rsquo;s receiving configuration</span></label><label class="inherit-option"><input type="checkbox" name="inherit_sending" value="1" checked> <span>Use <b class="add-domain-parent"></b>&rsquo;s sending configuration</span></label><input type="hidden" name="inherit_controls" id="add-domain-inherit-controls" value=""></div><div class="dialog-actions"><button type="button" class="secondary" id="add-domain-cancel">Cancel</button><button>Add Domain</button></div></form></dialog>
<dialog id="domain-delete-dialog" class="domain-dialog"><h2>Delete domain</h2><p class="muted">This permanently deletes <b class="confirm-name" id="domain-delete-name" data-copy="" role="button" tabindex="0" title="Click to copy"></b> <button type="button" class="secondary icon-btn copy-btn" id="domain-delete-copy" data-copy="" title="Copy name" aria-label="Copy domain name">{{copyIcon}}</button> and everything it owns — all of its inboxes, messages and attachments. This cannot be undone.</p><p class="muted small">Subdomains added under this domain are not deleted; they keep their own configuration.</p><div id="domain-delete-inboxes"><p class="muted small" id="domain-delete-inbox-summary"></p><div class="confirm-inbox-list" id="domain-delete-inbox-list" hidden></div></div><form method="post" id="domain-delete-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Type the domain name to confirm</label><input name="confirm" id="domain-delete-input" autocomplete="off" required><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button class="secondary danger" id="domain-delete-submit" disabled>Delete domain</button></div></form></dialog>
<dialog id="inbox-delete-dialog" class="domain-dialog"><h2>Delete inbox</h2><p class="muted">This permanently deletes <b class="confirm-name" id="inbox-delete-address" data-copy="" role="button" tabindex="0" title="Click to copy"></b> <button type="button" class="secondary icon-btn copy-btn" id="inbox-delete-copy" data-copy="" title="Copy address" aria-label="Copy address">{{copyIcon}}</button> and all of its messages and attachments. This cannot be undone.</p><form method="post" id="inbox-delete-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Type the email address to confirm</label><input name="confirm" id="inbox-delete-input" autocomplete="off" required><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button class="secondary danger" id="inbox-delete-submit" disabled>Delete inbox</button></div></form></dialog>
<dialog id="client-delete-dialog" class="domain-dialog"><h2>Delete client</h2><p class="muted">This permanently deletes <b class="confirm-name" id="client-delete-name" data-copy="" role="button" tabindex="0" title="Click to copy"></b> <span class="muted" id="client-delete-type"></span> <button type="button" class="secondary icon-btn copy-btn" id="client-delete-copy" data-copy="" title="Copy name" aria-label="Copy client name">{{copyIcon}}</button> and revokes its access. This cannot be undone.</p><form method="post" id="client-delete-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="inbox" value=""><label>Type the client name to confirm</label><input name="confirm" id="client-delete-input" autocomplete="off" required><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button class="secondary danger" id="client-delete-submit" disabled>Delete client</button></div></form></dialog>
<dialog id="inbox-dialog" class="inbox-settings"><div class="inbox-settings-head"><h2>Add inbox</h2><button type="button" class="secondary icon-btn inbox-close" aria-label="Close"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></div><div class="inbox-settings-body"><nav class="inbox-settings-nav" data-inbox-tabs role="tablist"><button type="button" class="dialog-tab active" role="tab" aria-selected="true" data-inbox-tab="basic" data-inbox-mode="domain" hidden>Identity</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="allow" data-inbox-mode="domain" hidden>Allow list</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="approver" data-inbox-mode="domain" hidden>Approver</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="quota" data-inbox-mode="domain" hidden>Quota</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="aliases" data-inbox-mode="domain" hidden>Alias</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="sa-basic" data-inbox-mode="standalone" hidden>Identity</button></nav><div class="inbox-settings-panels"><section class="dialog-panel" role="tabpanel" aria-label="Choose inbox type" data-inbox-panel="choose" data-inbox-choose><p class="muted">What kind of inbox do you want to add?</p><div class="add-inbox-choose"><button type="button" class="add-inbox-choice" data-choose="domain"><svg class="choice-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v11a2.5 2.5 0 0 1-2.5 2.5h-11A2.5 2.5 0 0 1 4 17.5z"/><path d="m5 7 7 5.5L19 7"/></svg><span class="choice-title">Domain inbox</span><span class="choice-desc">An address on one of your managed domains. Mammoth Mail receives and sends the mail over its own connectors (MX, Mailgun, Resend&hellip;).</span></button><button type="button" class="add-inbox-choice" data-choose="standalone"><svg class="choice-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4M7 9h.01M11 9h6"/></svg><span class="choice-title">Standalone inbox</span><span class="choice-desc">An existing mailbox elsewhere, connected through IMAP and optional SMTP. Mammoth Mail mirrors it live.</span></button></div></section><section class="dialog-panel" role="tabpanel" aria-label="Choose a provider" data-inbox-panel="sa-provider" hidden><p class="muted">Which provider hosts this mailbox?</p><div class="provider-choose"><button type="button" class="provider-choice" data-standalone-provider="google"><span class="provider-logo" aria-hidden="true"><svg viewBox="0 0 48 48"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg></span><span class="provider-text"><span class="provider-title">Google / Gmail</span><span class="provider-desc">Connect a Gmail or Google Workspace mailbox using your own Google Cloud app.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button><button type="button" class="provider-choice" data-standalone-provider="microsoft"><span class="provider-logo" aria-hidden="true"><svg viewBox="0 0 24 24"><rect x="1" y="1" width="10" height="10" fill="#F25022"/><rect x="13" y="1" width="10" height="10" fill="#7FBA00"/><rect x="1" y="13" width="10" height="10" fill="#00A4EF"/><rect x="13" y="13" width="10" height="10" fill="#FFB900"/></svg></span><span class="provider-text"><span class="provider-title">Microsoft / Outlook</span><span class="provider-desc">Connect a personal or shared Microsoft 365 / Outlook mailbox.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button><button type="button" class="provider-choice" data-standalone-provider="imap"><span class="provider-logo" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4M7 9h.01M11 9h6"/></svg></span><span class="provider-text"><span class="provider-title">IMAP + SMTP</span><span class="provider-desc">Any other mailbox, connected with IMAP and optional SMTP.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button></div></section><form id="inbox-add-form" method="post" action="/ui/inboxes"><input type="hidden" name="_csrf" value="{{.CSRF}}"><section class="dialog-panel" role="tabpanel" data-inbox-panel="basic" data-inbox-mode="domain" hidden><label>Email address</label><div class="email-field"><input name="local" placeholder="hermes" required><span class="at">@</span><select name="domain" required>{{range .Domains}}<option value="{{.ID}}" data-mx="{{if index $.DomainIsMX .ID}}1{{end}}">{{.Name}}</option>{{end}}</select></div><label>Display Name</label><input name="display" placeholder="Hermes"></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="allow" data-inbox-mode="domain" hidden><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="sender_restricted" value="1" id="inbox-add-sender-restricted" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Block senders to this inbox except the allow list below</span></label><div id="inbox-add-sender-section" hidden><p class="muted" id="inbox-add-sender-note">Only these From addresses are accepted. The From header can be spoofed, so this is a filter, not proof of identity.</p><ul id="inbox-add-sender-list" class="slist"><li class="empty">Add an address below to allow it to email this inbox.</li></ul><div class="row"><input id="inbox-add-sender-input" type="text" placeholder="someone@example.com"><button type="button" class="secondary btn-narrow" id="inbox-add-sender-add">Add</button></div><p class="muted small" id="inbox-add-sender-hint">Use <code>*@example.com</code> to allow any sender at a domain, or <code>*@*.example.com</code> for its subdomains.</p></div><div id="inbox-add-require-auth-section" hidden><h3 class="section-head">MX delivery</h3><p class="muted small">This domain receives mail by direct SMTP (MX). Require authenticated senders (SPF, DKIM or DMARC pass) in addition to the allow list.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="require_authenticated" value="1" id="inbox-add-require-auth" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Require an authenticated sender</span></label></div></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="approver" data-inbox-mode="domain" hidden><label>Assistant approver email</label><input name="approver_email" id="inbox-add-approver-email" placeholder="Optional — approves draft sends by email"><p class="muted small" id="inbox-add-approver-note">When set, this address will receive approval requests for emails requested to send by clients with Assistant permission.</p></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="quota" data-inbox-mode="domain" hidden><p class="muted">Storage is limited at the account level.</p><dl class="dialog-usage"><dt>This inbox</dt><dd>No usage yet — new inbox.</dd><dt>Account used</dt><dd>{{filesize .Account.StorageUsedBytes}} of {{filesize .Account.StorageQuotaBytes}}</dd></dl><h3 class="section-head">Inbox storage quota</h3><p class="muted small">An optional cap on this inbox's stored bytes, on top of the account limit. Inbound and outbound mail is refused once the inbox reaches it. Enter 0 for no inbox cap.</p><div class="row"><input id="inbox-add-quota-value" name="quota_value" type="number" min="0" step="any" placeholder="0"><select id="inbox-add-quota-unit" name="quota_unit" style="flex:0 0 96px"><option value="b">B</option><option value="kb">KB</option><option value="mb" selected>MB</option><option value="gb">GB</option><option value="tb">TB</option></select></div><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="quota_unlimited" value="1" id="inbox-add-quota-unlimited" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Leave this inbox uncapped (account limit still applies)</span></label></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="aliases" data-inbox-mode="domain" hidden><h3 class="section-head">Alias</h3><p class="muted">Alternate addresses that deliver to this inbox and that this inbox can send from. An alias may be on any domain in this account. Each alias has a sender name and an email address.</p><button type="button" class="secondary btn-narrow" id="inbox-add-alias-add">Add alias</button><ul id="inbox-add-alias-list" class="slist aliases"><li class="empty">No aliases.</li></ul><div class="alias-editor" id="inbox-add-alias-editor" hidden><div class="error" id="inbox-add-alias-error" hidden></div><label>Name</label><input id="inbox-add-alias-name" type="text" placeholder="Acme Sales" maxlength="128"><label>Email address</label><div class="email-field"><input id="inbox-add-alias-local" type="text" placeholder="sales"><span class="at">@</span><select id="inbox-add-alias-domain">{{range .Domains}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select></div><div class="dialog-actions"><button type="button" class="secondary" id="inbox-add-alias-cancel">Cancel</button><button type="button" id="inbox-add-alias-save">Add</button></div></div><h3 class="section-head">Primary / Default Address</h3><select name="default_sender" id="inbox-add-default-sender"><option value="">Primary address</option></select></section></form><section class="dialog-panel" role="tabpanel" data-inbox-panel="sa-google-advanced" data-inbox-mode="standalone" hidden><h2 class="wizard-step-title"><span class="wizard-logo" aria-hidden="true"><svg viewBox="0 0 48 48"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg></span>Google / Gmail</h2><p class="muted">Google Gmail setup can be quite complex and is an advanced-user function. It needs your own Google Cloud OAuth app, which you create and verify yourself. You may find it easier to connect with either <b>One</b> or <b>IMAP</b>, which are easier wizards.</p><p class="muted"><b>Are you sure you want to continue with Google Gmail?</b></p><div class="provider-choose"><button type="button" class="provider-choice provider-choice-advanced" data-google-advanced-continue><span class="provider-logo" aria-hidden="true"><svg viewBox="0 0 48 48"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg></span><span class="provider-text"><span class="provider-title">Continue with Google Gmail</span><span class="provider-desc">Set up your own Google Cloud app and connect with OAuth. For advanced users.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button><!--ONE_ADVANCED_BUTTON--><button type="button" class="provider-choice provider-choice-advanced" data-google-advanced-imap><span class="provider-logo" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4M7 9h.01M11 9h6"/></svg></span><span class="provider-text"><span class="provider-title">Use IMAP + SMTP instead</span><span class="provider-desc">Connect this mailbox directly over IMAP and optional SMTP. Simpler, and works with any provider.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button></div><p class="muted small">Gmail connections stay in Google's app-verification flow: a personal @gmail.com account must keep the app in Testing and register the account as a test user.</p></section><form id="inbox-add-standalone-form" method="post" action="/ui/inboxes/standalone" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}"><section class="dialog-panel" role="tabpanel" data-inbox-panel="sa-basic" data-inbox-mode="standalone" hidden><h3 class="section-head">Identity</h3><label>Email address</label><input name="address" placeholder="agent@example.com" required><label>Display name</label><input name="display" placeholder="Agent"><p class="muted small">A standalone mailbox owns an address independent of any managed domain. The remote server connection below is optional and can be configured later.</p><details class="remote-fields" id="inbox-add-remote-imap" open><summary>Remote server (IMAP)</summary><div class="remote-fields-body"><p class="muted small">Connect this mailbox over IMAP. Leave the host blank to create the inbox now and configure the connector later. Prefer Google or Microsoft sign-in? Go back and choose that provider.</p><label>IMAP host</label><input name="host" placeholder="imap.example.com"><label>IMAP port</label><input name="port" type="number" min="1" max="65535" placeholder="993"><label>IMAP username</label><input name="username" placeholder="agent@example.com"><label>IMAP password or app password</label><input name="imap_password" type="password" autocomplete="new-password" placeholder="app password"><label>Security</label><select name="security"><option value="tls" selected>TLS (implicit, default)</option><option value="starttls">STARTTLS</option><option value="plain">Plain (no transport security)</option></select><p class="muted small">TLS is the default and the connection never downgrades. Plain is an explicit choice for a self-hosted server; the deployment may refuse it.</p><label>Sync root folder</label><input name="namespace" placeholder="INBOX"><p class="muted small">The root folder this inbox syncs. The default personal root is INBOX plus its siblings. A missing special folder (Sent, Drafts, Trash, Spam) is mapped on first sync, or you can select or create one from the inbox settings.</p></div></details><details class="remote-fields" id="inbox-add-remote-smtp"><summary>Outbound SMTP (optional)</summary><div class="remote-fields-body"><p class="muted small">Optional. Without an SMTP server this inbox can receive and hand off drafts but cannot send from Mammoth Mail. Leave the host blank to configure it later.</p><label>SMTP host</label><input name="smtp_host" placeholder="smtp.example.com"><label>SMTP port</label><input name="smtp_port" type="number" min="1" max="65535" placeholder="465"><label>SMTP username</label><input name="smtp_username" placeholder="agent@example.com"><label>SMTP security</label><select name="smtp_security"><option value="tls" selected>TLS (implicit, default)</option><option value="starttls">STARTTLS</option><option value="plain">Plain (no transport security)</option></select><label>SMTP password or app password</label><input name="smtp_password" type="password" autocomplete="new-password" placeholder="app password"></div></details></section></form></div></div><footer class="inbox-settings-footer"><button type="button" class="secondary" id="inbox-back" hidden>&larr; Back</button><span class="spacer"></span><button type="button" class="secondary" id="inbox-cancel">Cancel</button><button type="submit" id="inbox-add-submit" form="inbox-add-form" hidden>Add Inbox</button></footer></dialog>
<dialog id="inbox-edit-dialog" class="inbox-settings"><div class="inbox-settings-head"><h2>Edit inbox</h2><button type="button" class="secondary icon-btn inbox-close" aria-label="Close"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></div><div class="inbox-settings-body"><nav class="inbox-settings-nav" data-inbox-tabs role="tablist"><button type="button" class="dialog-tab active" role="tab" aria-selected="true" data-inbox-tab="basic">Identity</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="access">Clients &amp; Access</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="allow">Allow list</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="approver">Assistant Approver</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="quota">Quota</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="aliases">Alias</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="connectors">Connectors</button></nav>
<div class="inbox-settings-panels"><form id="inbox-edit-form" method="post"><input type="hidden" name="_csrf" value="{{.CSRF}}"><section class="dialog-panel" role="tabpanel" data-inbox-panel="basic"><div class="setting-row"><div class="setting-label"><label for="inbox-edit-display">Display name</label></div><input id="inbox-edit-display" name="display"></div><div class="setting-row"><div class="setting-label"><label for="inbox-edit-address">Email address</label></div><input id="inbox-edit-address" value="" disabled></div><div class="setting-row" id="inbox-dashboard-group" hidden><div class="setting-label"><label for="inbox-dashboard-group-input">Dashboard group</label><p class="muted small">A label on your dashboard only — it is personal to you and does not affect access, routing or quota.</p></div><input name="dashboard_group" id="inbox-dashboard-group-input" maxlength="64" placeholder="e.g. Agents" autocomplete="off"></div><div class="inbox-schedule" id="inbox-sync-schedule" data-remote-schedule hidden><p class="muted small" id="inbox-schedule-note">New mail is pushed instantly when the server supports IDLE; these intervals govern the fallback poll and the periodic full sync. Enter 0 to turn the new-mail poll off and rely on push and the full sync.</p><div class="inbox-schedule-grid"><label>Check for new mail every (minutes)<input name="remote_poll_minutes" id="inbox-remote-poll-minutes" type="number" min="0" max="1440" placeholder="1"></label><label>Full sync every (minutes)<input name="remote_full_sync_minutes" id="inbox-remote-full-sync-minutes" type="number" min="1" max="10080" placeholder="15"></label></div></div><details class="remote-fields" id="inbox-remote-imap" data-standalone-only hidden><summary>Remote server (IMAP)</summary><div class="remote-fields-body"><p class="muted" id="inbox-remote-configured">No connector configured.</p><label>IMAP host</label><input name="remote_host" id="inbox-remote-host" placeholder="imap.example.com"><label>IMAP port</label><input name="remote_port" id="inbox-remote-port" type="number" min="1" max="65535" placeholder="993"><label>IMAP username</label><input name="remote_username" id="inbox-remote-username" placeholder="agent@example.com"><label>IMAP password or app password <span class="muted small" id="inbox-remote-imap-pw-note">(leave blank to keep the current value)</span></label><input name="imap_password" id="inbox-remote-imap-pw" type="password" autocomplete="new-password" placeholder="app password"><label>Security</label><select name="remote_security" id="inbox-remote-security"><option value="tls" selected>TLS (implicit, default)</option><option value="starttls">STARTTLS</option><option value="plain">Plain (no transport security)</option></select><p class="muted small">TLS is the default and the connection never downgrades. Plain is an explicit choice for a self-hosted server; the deployment may refuse it. Saving runs a live sign-in test and keeps the existing connector when it fails.</p><label>Sync root folder</label><input name="namespace" id="inbox-remote-namespace" placeholder="INBOX"><p class="muted small">The root folder this inbox syncs. Map special folders and the sent copy from the full <a id="inbox-remote-settings-link" href="#">connection settings</a> page.</p></div></details><details class="remote-fields" id="inbox-remote-smtp" data-standalone-only hidden><summary>Outbound SMTP (optional)</summary><div class="remote-fields-body"><p class="muted small">Without an SMTP server this inbox can receive and hand off drafts but cannot send from Mammoth Mail. Leave the host blank to disable outbound.</p><label>SMTP host</label><input name="smtp_host" id="inbox-remote-smtp-host" placeholder="smtp.example.com"><label>SMTP port</label><input name="smtp_port" id="inbox-remote-smtp-port" type="number" min="1" max="65535" placeholder="465"><label>SMTP username</label><input name="smtp_username" id="inbox-remote-smtp-username" placeholder="agent@example.com"><label>SMTP security</label><select name="smtp_security" id="inbox-remote-smtp-security"><option value="tls" selected>TLS (implicit, default)</option><option value="starttls">STARTTLS</option><option value="plain">Plain (no transport security)</option></select><label>SMTP password or app password <span class="muted small" id="inbox-remote-smtp-pw-note">(leave blank to keep the current value)</span></label><input name="smtp_password" id="inbox-remote-smtp-pw" type="password" autocomplete="new-password" placeholder="app password"></div></details></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="allow" hidden><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="sender_restricted" value="1" id="inbox-sender-restricted" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Block senders to this inbox except the allow list below</span></label><div id="inbox-sender-section" hidden><p class="muted" id="inbox-sender-note">Only these From addresses are accepted. The From header can be spoofed, so this is a filter, not proof of identity.</p><ul id="inbox-sender-list" class="slist"><li class="empty">Add an address below to allow it to email this inbox.</li></ul><div class="row"><input id="inbox-sender-input" type="text" placeholder="someone@example.com"><button type="button" class="secondary btn-narrow" id="inbox-sender-add">Add</button></div><p class="muted small" id="inbox-sender-hint">Use <code>*@example.com</code> to allow any sender at a domain, or <code>*@*.example.com</code> for its subdomains.</p></div><div id="inbox-edit-require-auth-section" hidden><h3 class="section-head">MX delivery</h3><p class="muted small">This domain receives mail by direct SMTP (MX). Require authenticated senders (SPF, DKIM or DMARC pass) in addition to the allow list.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="require_authenticated" value="1" id="inbox-require-auth" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Require an authenticated sender</span></label></div></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="approver" hidden><h3 class="section-head">Approvals</h3><p class="muted small" id="inbox-authoring-desc">How this inbox handles a request to send. Mammoth Mail approval keeps the draft in Mammoth Mail for an in-app decision (and, if an approver is set, a tokenized email). Remote draft places the draft in the connected mailbox's Drafts folder and never sends it.</p><div class="inbox-authoring"><div id="inbox-authoring-controls" hidden><label>Mode</label><select name="authoring_mode" id="inbox-authoring-mode"><option value="local_approval">Mammoth Mail approval</option><option value="remote_draft">Create draft</option></select></div></div><div id="inbox-approver-fields"><h3 class="section-head">Assistant Approver</h3><label>Assistant approver email</label><input name="approver_email" id="inbox-edit-approver-email" placeholder="Optional — approves draft sends by email"><p class="muted small" id="inbox-approver-note">When set, this address will receive approval requests for emails requested to send by clients with Assistant permission. The approver applies to Mammoth Mail approval only; a Create draft inbox has no in-app approver.</p></div></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="quota" hidden><p class="muted">Storage is limited at the account level.</p><dl class="dialog-usage"><dt>This inbox</dt><dd id="inbox-edit-usage">—</dd><dt>Account used</dt><dd>{{filesize .Account.StorageUsedBytes}} of {{filesize .Account.StorageQuotaBytes}}</dd></dl><h3 class="section-head">Inbox storage quota</h3><p class="muted small">An optional cap on this inbox's stored bytes, on top of the account limit. Inbound and outbound mail is refused once the inbox reaches it. Enter 0 for no inbox cap.</p><div class="row"><input id="inbox-edit-quota-value" name="quota_value" type="number" min="0" step="any" placeholder="0"><select id="inbox-edit-quota-unit" name="quota_unit" style="flex:0 0 96px"><option value="b">B</option><option value="kb">KB</option><option value="mb" selected>MB</option><option value="gb">GB</option><option value="tb">TB</option></select></div><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="quota_unlimited" value="1" id="inbox-edit-quota-unlimited" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Leave this inbox uncapped (account limit still applies)</span></label><h3 class="section-head">Trash retention</h3><p class="muted small">How long this inbox keeps deleted messages before they are purged. By default it follows the account setting.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="trash_retention_override" value="1" id="inbox-trash-retention-override" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Override the account default for this inbox</span></label><div id="inbox-trash-retention-section" hidden><label>Auto-purge trashed messages after (days)</label><input type="number" name="trash_retention_days" id="inbox-trash-retention-days" value="0" min="0" max="3650"><p class="muted small">Set to 0 to keep this inbox's trashed messages until you empty the trash manually.</p></div></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="aliases" hidden><h3 class="section-head">Alias</h3><p class="muted">Alternate addresses that deliver to this inbox and that this inbox can send from. An alias may be on any domain in this account. Each alias has a sender name and an email address.</p><button type="button" class="secondary btn-narrow" id="inbox-alias-add">Add alias</button><ul id="inbox-alias-list" class="slist aliases"><li class="empty">No aliases.</li></ul><div class="alias-editor" id="inbox-alias-editor" hidden><div class="error" id="inbox-alias-editor-error" hidden></div><label>Name</label><input id="inbox-alias-editor-name" type="text" placeholder="Acme Sales" maxlength="128"><label>Email address</label><div class="email-field"><input id="inbox-alias-editor-local" type="text" placeholder="sales"><span class="at">@</span><select id="inbox-alias-editor-domain">{{range .Domains}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select></div><div class="dialog-actions"><button type="button" class="secondary" id="inbox-alias-editor-cancel">Cancel</button><button type="button" id="inbox-alias-editor-save">Add</button></div></div><h3 class="section-head">Primary / Default Address</h3><select name="default_sender" id="inbox-default-sender"><option value="">Primary address</option></select></section></form><form method="post" id="inbox-connectors-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><section class="dialog-panel" role="tabpanel" data-inbox-panel="connectors" hidden><div class="card-head"><div><h3 class="section-head">Connectors</h3><p class="muted small">Hermes Relay and Webhook connections attached to this inbox.</p></div><button type="button" class="secondary btn-sm add-connector" id="inbox-connector-add" data-inbox="">Add Connector</button></div><div id="inbox-connectors-list" class="connector-list"></div><h3 class="section-head">After a connector handles mail</h3><p class="muted small">Automatic actions once a connector has received a message.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="auto_mark_read_on_delivery" value="1" id="inbox-auto-mark-read" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Mark messages read once delivered</span></label><label style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="auto_trash_after_delivery" value="1" id="inbox-auto-trash" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Move messages to Trash after delivery</span></label><div id="inbox-auto-trash-section" hidden><label>Trash after (hours)</label><input type="number" name="auto_trash_after_delivery_hours" id="inbox-auto-trash-hours" value="24" min="1" max="87600"></div><label>Trigger when</label><select name="delivery_trigger" id="inbox-delivery-trigger"><option value="any">Any connector has delivered</option><option value="all" selected>All connectors present at receipt have delivered</option></select></section></form><section class="dialog-panel" role="tabpanel" data-inbox-panel="access" hidden><div class="card-head"><div><h3 class="section-head">Clients</h3><p class="muted small">API keys with access to this inbox. Account Admin keys have implicit Owner on every inbox.</p></div><button type="button" class="secondary btn-sm" id="access-add-key">Add client</button></div><div id="inbox-access-keys" class="access-list"></div><h3 class="section-head">Mailbox users</h3><p class="muted small">People who can sign in to this inbox. They are always Owner of the mailboxes you give them.</p><button type="button" class="secondary btn-sm" id="access-add-user">Add mailbox user</button><div id="inbox-access-users" class="access-list"></div><h3 class="section-head">Pending invitations</h3><div id="inbox-access-invites" class="access-list"></div></section>
<div class="inbox-subview" data-inbox-subview="access-add"><div class="inbox-subview-head"><button type="button" class="secondary btn-sm inbox-subview-back">← Back</button><h3 id="access-add-title">Add client</h3></div><div id="access-add-body"></div></div>
<div class="inbox-subview" data-inbox-subview="connectors"><div class="inbox-subview-head"><button type="button" class="secondary btn-sm inbox-subview-back">← Back</button><h3>Connector</h3></div><div id="inbox-connector-editor" class="connector-editor"></div></div></div></div><footer class="inbox-settings-footer"><button type="button" class="secondary danger open-delete-inbox" id="inbox-edit-delete" data-id="" data-address="">Delete Inbox</button><span class="spacer"></span><button type="button" class="secondary" id="inbox-edit-cancel">Cancel</button><button type="submit" form="inbox-edit-form" id="inbox-edit-save">Save</button></footer></dialog>
{{range .Domains}}{{$d := .}}{{$sel := index $.DomainSendingSelected .ID}}<dialog id="domain-sending-dialog-{{.ID}}" class="domain-dialog{{if index $.DomainSendingMX $d.ID}} domain-dialog--wide{{end}}"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "sending")}} data-open="1"{{end}}><h2>Sending · {{.Name}}</h2><form id="domain-sending-form-{{$d.ID}}" method="post" action="/ui/domains/{{$d.ID}}/sending" class="cfg-form" autocomplete="off" data-sending-domain="{{$d.ID}}"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><label>Provider</label><select name="provider" class="provider-select"><option value="">Select a provider…</option>{{if $d.ParentDomainID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{$d.ParentDomain}})</option>{{else if index $.DomainParentCandidate $d.ID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{index $.DomainParentCandidate $d.ID}})</option>{{end}}{{range index $.DomainSendingEditors $d.ID}}<option value="{{.Provider}}"{{if eq .Provider $sel}} selected{{end}}>{{.ProviderLabel}}</option>{{end}}</select><p class="muted provider-hint"{{if $sel}} hidden{{end}}>Choose a provider to configure sending.</p>{{range index $.DomainSendingEditors $d.ID}}{{$e := .}}<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>{{if .Error}}<div class="error">{{.Error}}</div>{{end}}{{if .KeepSecrets}}<p class="muted">Saving {{.ProviderLabel}} updates this domain's sending configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted">Saving {{.ProviderLabel}} replaces this domain's sending configuration. Required secrets must be entered.</p>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{else if .Overridable}}{{if and .Secret $e.KeepSecrets}}<label>{{.Label}} <span class="muted small">(leave blank to keep the current key)</span></label>{{else}}<label>{{.Label}}</label>{{end}}<input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}} autocomplete="off">{{end}}{{end}}{{if .HasOverridable}}<p class="muted small">{{.OverridableHint}}</p>{{end}}{{with .DKIM}}{{$k := .}}<section class="dkim-panel" data-dkim-panel data-show-field="cfg_mx_dkim_enabled" data-show-values="on"{{if not $k.Enabled}} hidden{{end}}><div class="dkim-head"><h3 class="section-head">DNS record</h3>{{with index $.DomainSendingLight $d.ID}}<p class="dkim-status" data-dkim-record-status><span class="dns-light {{.Light}}" data-dkim-record-light title="{{.Title}}" aria-label="{{.Title}}"></span> <b class="dkim-status-text" data-dkim-record-text>{{dkimStatus .Light}}</b></p>{{end}}</div>{{if $k.HasKey}}<p class="muted small">Mammoth Mail signs outgoing mail for this domain with the key below. Add this record at your DNS provider so receivers can verify it. Until it is published, mail is still sent but fails the DKIM check.</p><div class="dns-remediation-row"><span class="dns-remediation-label">Type</span><code class="dns-remediation-value dns-value-short">TXT</code><span class="dns-remediation-label">Name</span><code class="dns-remediation-value">{{$k.RecordName}}</code><button type="button" class="secondary" data-copy-value="{{$k.RecordName}}">Copy</button></div><div class="dns-remediation-row"><span class="dns-remediation-label">Value</span><code class="dns-remediation-value dns-value-long">{{$k.RecordValue}}</code><button type="button" class="secondary" data-copy-value="{{$k.RecordValue}}">Copy</button></div><p class="muted small">Selector <code>{{$k.Selector}}</code> · signing as <code>{{$k.Domain}}</code>. If your DNS provider appends the zone name, enter just <code>{{$k.Selector}}._domainkey</code>.</p><div class="dkim-reveal"><button type="submit" class="linklike" form="dkim-reveal-form-{{$d.ID}}">Show private key</button></div>{{else}}<p class="muted small">Mammoth Mail will sign outgoing mail for this domain with a key it generates. Save to create the key and show the DKIM record to publish.</p>{{end}}</section>{{end}}</div>{{end}}</form>{{with index $.DomainSendingDKIMRevealURL $d.ID}}<form id="dkim-reveal-form-{{$d.ID}}" method="post" action="{{.}}"><input type="hidden" name="_csrf" value="{{$.CSRF}}"></form>{{end}}<div class="dialog-actions">{{if $d.SendingProvider}}<div class="dialog-danger">{{if index $.DomainSendingDKIM $d.ID}}<form method="post" action="/ui/domains/{{$d.ID}}/sending/dkim/rotate" data-confirm="Rotate the DKIM signing key for {{$d.Name}}? Mammoth Mail will sign with a brand new key immediately, and this domain's existing DKIM TXT record will no longer match. You must publish the new record value at the same name before receivers can verify the domain's mail; until then its messages fail the DKIM check."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary">Rotate key</button></form>{{end}}<form method="post" action="/ui/domains/{{$d.ID}}/sending/clear" data-confirm="Remove sending configuration for this domain? Mail will queue until a provider is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove sending</button></form></div>{{end}}<button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="domain-sending-form-{{$d.ID}}" data-save-provider{{if not $sel}} disabled{{end}}>Save</button></div></dialog>
{{end}}<style>
.antler-wizard [hidden]{display:none!important}.antler-progress{font-size:13px;color:#666;border-bottom:1px solid #ddd;padding-bottom:8px}.antler-intro{margin:0 0 14px}.antler-intro p{margin:0 0 10px}.antler-intro figure{margin:12px 0}.antler-intro img{display:block;width:100%;height:auto;border:1px solid #ddd;border-radius:8px;background:#fff}.antler-dns-table{width:100%;border-collapse:collapse;font-size:13px;table-layout:fixed}.antler-dns-table th{text-align:left;color:#666;font-weight:600;border-bottom:1px solid #ddd;padding:6px}.antler-dns-table td{vertical-align:middle;padding:8px 6px;border-bottom:1px solid #eee;overflow-wrap:anywhere}.antler-dns-table th:nth-child(1),.antler-dns-table td:nth-child(1){width:48%}.antler-dns-table th:nth-child(2),.antler-dns-table td:nth-child(2),.antler-dns-table th:nth-child(3),.antler-dns-table td:nth-child(3){width:26%}.antler-dns-remediation{margin:8px 0;padding:8px 10px;background:#fff8e1;border:1px solid #f1d58a;border-radius:4px;overflow-wrap:anywhere}.antler-dns-remediation summary{cursor:pointer;font-weight:600}.antler-dns-remediation p{margin:8px 0;font-size:12px}.antler-settings{margin-top:14px;padding-top:10px;border-top:1px solid #eee}.antler-settings h3{margin:0 0 4px}.antler-settings label{display:block;margin-top:8px;font-size:13px}.antler-settings input,.antler-settings select{display:block;margin-top:2px}.antler-dns-remediation button{padding:3px 8px;font-size:12px}.dns-remediation-row{display:flex;align-items:center;gap:8px;margin:4px 0;flex-wrap:wrap}.dns-remediation-label{flex:0 0 88px;font-size:12px;font-weight:600;color:#666}.dns-remediation-value{flex:1 1 220px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;background:#fff;border:1px solid #e6d9a8;border-radius:3px;padding:3px 6px;overflow-wrap:anywhere}.antler-light{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:8px}.antler-light.green{background:#188038}.antler-light.amber{background:#f9ab00}.antler-light.red{background:#b3261e}.antler-checks{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.antler-check-note{font-size:12px}.antler-manifest-note{flex-basis:100%;margin:0;font-size:12px}.antler-check-note[aria-busy="true"]:before{content:"";display:inline-block;width:12px;height:12px;border:2px solid #ccc;border-top-color:#1557b0;border-radius:50%;margin-right:8px;animation:antler-spin 1s linear infinite}.antler-checking .antler-light.amber{box-sizing:border-box;background:transparent;border:2px solid rgba(249,171,0,.35);border-top-color:#f9ab00;animation:antler-spin 1s linear infinite}@keyframes antler-spin{to{transform:rotate(360deg)}}@media(prefers-reduced-motion:reduce){.antler-check-note[aria-busy="true"]:before{animation:none}.antler-checking .antler-light.amber{animation:none;background:#f9ab00;border:0}}
</style>{{range .Domains}}{{$d := .}}{{$sel := index $.DomainReceivingSelected .ID}}
<dialog id="domain-receiving-dialog-{{.ID}}" class="domain-dialog{{if or (eq $sel "mx") (eq $sel "remotemx")}} domain-dialog--wide{{end}}"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "receiving")}} data-open="1"{{end}}>
<h2>Receiving · {{.Name}}</h2>
<style>
.antler-dialog .provider-box,.antler-dialog .antler-wizard,.antler-dialog .antler-records{max-height:none;overflow:visible}
.antler-dialog{width:min(900px,calc(100vw - 48px))}
.antler-dns-table th,.antler-dns-table td{width:auto!important}
.antler-status-table th:first-child,.antler-status-table td:first-child{width:48%!important}
.antler-record-table{table-layout:fixed}
.antler-record-table th:nth-child(1){width:20%!important}
.antler-record-table th:nth-child(2){width:7%!important}
.antler-record-table th:nth-child(3){width:8%!important}
.antler-record-table th:nth-child(4){width:39%!important}
.antler-record-table th:nth-child(5){width:12%!important}
.antler-record-table th:nth-child(6){width:14%!important}
.antler-record-table button{padding:4px 8px;font-size:12px;white-space:normal}
</style>
<form id="domain-receiving-form-{{$d.ID}}" method="post" action="/ui/domains/{{$d.ID}}/receiving" class="cfg-form" autocomplete="off" data-antler-domain="{{$d.ID}}" data-antler-domain-name="{{$d.Name}}" data-antler-parent="{{$d.ParentDomain}}"{{with index $.DialMXSetup $d.ID}} data-antler-connectors="{{.ConnectorsJSON}}" data-antler-status="{{.StatusesJSON}}" data-antler-instructions="{{.InstructionsJSON}}" data-antler-unexpected="{{.UnexpectedJSON}}" data-antler-dns="{{.DNSJSON}}" data-antler-manifest="{{.ManifestJSON}}"{{if .InheritedFrom}} data-antler-inherited="{{.InheritedFrom}}"{{end}}{{end}}>
<input type="hidden" name="_csrf" value="{{$.CSRF}}">
<label>Provider</label>
<select name="provider" class="provider-select"><option value="">Select a provider…</option>{{if $d.ParentDomainID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{$d.ParentDomain}})</option>{{else if index $.DomainParentCandidate $d.ID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{index $.DomainParentCandidate $d.ID}})</option>{{end}}{{range index $.DomainReceivingEditors $d.ID}}<option value="{{.Provider}}"{{if eq .Provider $sel}} selected{{end}} data-next="{{if .Generated}}1{{end}}">{{if .SelectLabel}}{{.SelectLabel}}{{else}}{{.ProviderLabel}}{{end}}</option>{{end}}</select>
<p class="muted provider-hint"{{if $sel}} hidden{{end}}>Choose a provider to configure receiving.</p>
{{if $.MXSetup}}{{$m := $.MXSetup}}<section class="mx-setup" data-mx-panel{{if ne $sel "mx"}} hidden{{end}}>
{{if $m.TargetHost}}<p class="muted small">Point the MX records for <code>{{$d.Name}}</code> at <code>{{$m.TargetHost}}</code>.</p>{{end}}
<div class="banner warn"><b>Shared receiver</b> · all Direct MX domains use these receiver settings.</div>
{{if not $m.Configured}}<div class="banner warn"><b>Not receiving mail.</b> The installation's bundled SMTP receiver has not been configured. Saving this domain's receiving provider will not start the SMTP receiver. {{if $.Principal.SystemAdmin}}Configure the bundled receiver below, then ensure public SMTP port 25 reaches this installation.{{else}}Ask a system administrator to configure the bundled receiver and public SMTP port 25.{{end}}</div>{{end}}
</section>{{end}}
{{range index $.DomainReceivingEditors $d.ID}}{{$e := .}}{{if or (not .Generated) .Steps .WebhookURL .HasOverridable}}
<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if and (eq .Provider "postmark") .KeepSecrets}}<p class="muted">Postmark credentials are already saved. The authenticated webhook URL was shown only once. If you lost it, use Regenerate secret below; the old credentials stop working immediately. Saving this form does not reveal existing credentials.</p>{{else if .Steps}}<ol class="steps">{{range $i, $step := .Steps}}<li>{{$step}}{{if and $e.WebhookURL (eq $i $e.WebhookStep)}}<div class="setup-url-row"><pre class="setup-webhook-url">{{$e.WebhookURL}}</pre><button type="button" class="secondary setup-copy icon-btn" title="Copy webhook URL" aria-label="Copy webhook URL">{{copyIcon}}</button></div>{{end}}</li>{{end}}</ol>{{end}}
{{range .Fields}}{{if or (not .Generated) .Overridable}}{{if and (eq $e.Provider "dialmx") (eq .Name "receiver_urls")}}{{$f := .}}{{if ne (index $e.Values "service") "custom"}}<template class="dialmx-receiver-urls-field"><div class="dialmx-receiver-urls"><label>{{$f.Label}}{{if $f.Required}} *{{end}}</label><input type="{{$f.Type}}" name="cfg_{{$e.Provider}}_{{$f.Name}}" placeholder="{{$f.Placeholder}}" value="{{index $e.Values $f.Name}}"></div></template>{{else}}<div class="dialmx-receiver-urls"><label>{{$f.Label}}{{if $f.Required}} *{{end}}</label><input type="{{$f.Type}}" name="cfg_{{$e.Provider}}_{{$f.Name}}" placeholder="{{$f.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if $f.Required}} required{{end}} value="{{index $e.Values $f.Name}}"></div>{{end}}{{else}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets (not (and .Generated .Overridable))}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Generated .Overridable}} autocomplete="off"{{else if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}{{end}}
{{if .HasOverridable}}{{if .KeepSecrets}}<p class="muted small">Optional. Leave blank to keep the current shared secret, or enter a secret to replace it.</p>{{else}}<p class="muted small">Optional. Enter an existing shared secret to reuse a Worker across domains, or leave blank to generate one.</p>{{end}}{{end}}
{{if not .HasOverridable}}{{if .KeepSecrets}}<p class="muted small">Saving updates this domain's receiving configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted small">Saving replaces this domain's receiving configuration. Required secrets must be entered.</p>{{end}}{{end}}
</div>
{{end}}{{end}}
{{if $.MXSetup}}{{$m := $.MXSetup}}{{if $.Principal.SystemAdmin}}{{with index $m.Editors $d.ID}}<section class="mx-setup" data-mx-panel{{if ne $sel "mx"}} hidden{{end}}>{{template "mx-editor" .}}</section>{{end}}{{end}}{{end}}
{{if $.RemoteMXSetup}}{{$r := $.RemoteMXSetup}}<section class="mx-setup" data-remote-mx-panel data-provider="remotemx"{{if ne $sel "remotemx"}} hidden{{end}}>
<p class="muted small">Set up a receiver you run separately. Mammoth Mail connects outbound to it over HTTP/2. Follow the <a href="https://github.com/dellarb/mammothmail/blob/main/docs/REMOTE_MX.md#remote-mx-one-receiver-for-an-account" target="_blank" rel="noopener noreferrer">Remote SMTP receiving guide</a>, then enter the receiver URL and bearer key below.</p>
<p class="muted small">The receiver's SMTP hostname, limits, verification and TLS are configured on the receiver itself, not here. Point the MX records for <code>{{$d.Name}}</code> at that hostname; no per-domain DNS proof or key is needed.</p>
<p class="muted small" data-remote-mx-status>Receiver status: {{if $r.Configured}}{{if $r.State}}{{$r.State}}{{else}}unknown{{end}}{{if $r.Detail}} — {{$r.Detail}}{{end}}{{else}}Not configured{{end}}</p>
{{if $.Principal.Admin}}<div class="remote-mx-editor">
<p class="muted small">The receiver connection is shared by this account's Remote MX domains. Save updates the connection and this domain's receiving settings together.</p>
{{if $r.Error}}<div class="error">{{$r.Error}}</div>{{end}}
<input type="hidden" name="rx_revision" value="{{$r.Revision}}">
<label>Receiver URL</label>
<input name="rx_url" placeholder="https://mx.example.com" value="{{$r.URL}}">
<p class="muted small">Use the session URL of your standalone receiver.</p>
<label>Bearer key{{if $r.KeyConfigured}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label>
<input name="rx_bearer_key" type="password" autocomplete="off" placeholder="{{if $r.KeyConfigured}}unchanged{{else}}the receiver's DIALMX_CORE_KEY{{end}}">
<p class="muted small">This must match <code>DIALMX_CORE_KEY</code> on the receiver. The saved key is never displayed.</p>
<details class="remote-mx-advanced"><summary>Advanced network and TLS settings</summary>
<label class="inherit-option"><input type="checkbox" name="rx_allow_private" value="1"{{if $r.AllowPrivate}} checked{{end}}> <span>Allow a private / LAN receiver (loopback or RFC1918 destination)</span></label>
<p class="muted small">Only honoured when the operator allows private outbound (<code>ALLOW_PRIVATE_OUTBOUND=true</code>, the default).</p>
<label>Private CA certificate bundle (PEM, optional)</label>
<textarea name="rx_ca" rows="3" placeholder="-----BEGIN CERTIFICATE-----">{{$r.CA}}</textarea>
<p class="muted small">Only needed when the receiver's TLS certificate is signed by a CA this server does not already trust. This certificate verifies TLS; the bearer key authenticates the connection.</p>
</details>
</div>{{end}}
</section>{{end}}
</form>
{{with index $.DialMXSetup $d.ID}}<section class="dialmx-setup">
<h3 class="section-head">{{if eq .Service "hosted"}}Tusk MX{{else}}Dial MX{{end}} key &amp; DNS</h3>
{{if .ContactEmail}}<p class="muted small">Contact email: <code>{{.ContactEmail}}</code></p>{{end}}
{{if .ReceiverURLs}}<p class="muted small">Receiver URLs: <code>{{.ReceiverURLs}}</code></p>{{end}}
{{if .MX}}<p><b>Publish these MX records for <code>{{$d.Name}}</code>:</b></p>
<pre class="dialmx-txt">{{range .MX}}{{.Priority}} {{.Hostname}}
{{end}}</pre>{{end}}
<p><b>Publish this TXT record at <code>_mail-proof.{{$d.Name}}</code>:</b></p>
<pre class="dialmx-txt">{{.TXT}}</pre>
<p class="muted small">Key ID: <code>{{.KeyID}}</code> · Public key: <code>{{.PublicKey}}</code></p>
{{if .DNS}}<ul class="dialmx-dns">{{range .DNS}}<li><span class="dns-light {{dnsLight .}}" aria-hidden="true"></span> {{if eq .Kind "mx"}}MX{{else}}TXT{{end}} <code>{{.Name}}</code> — {{if eq .State "ok"}}published and matching{{else if eq .State "mismatch"}}{{.Reason}}{{else}}{{.Reason}}{{end}}{{if .Found}} <span class="muted small">(found: {{join .Found ", "}})</span>{{end}}</li>{{end}}</ul>{{end}}
{{if .Statuses}}<dl class="dialmx-status">{{range .Statuses}}<dt>{{.ReceiverURL}}</dt><dd>{{if dialmxReady .}}<span class="pill">ready</span>{{if .SMTPHostname}} · receiving at <code>{{.SMTPHostname}}</code>{{end}}{{if dialmxExpiry .}} · rekey in {{dialmxExpiry .}}{{end}}{{else}}<span class="pill {{dialmxStatusClass .}}">{{dialmxStatusLabel .}}</span>{{if .HTTPCode}} · HTTP {{.HTTPCode}}{{else if .Reason}} · {{.Reason}}{{end}}{{end}}</dd>{{end}}</dl>{{else}}<p class="muted small">No live receiver status yet. Save the configuration, then authorization and MX routing are confirmed automatically.</p>{{end}}
<p class="copy-note">After regenerating the key, replace this TXT record too. Authorization is re-established within about five minutes.</p>
</section>{{end}}
<div class="dialog-actions">{{/* The danger block is drawn for every receiving dialog and hidden while nothing is stored, so the Dial MX wizard can reveal Remove receiving the moment it stores a configuration. */}}<div class="dialog-danger"{{if and (not $d.ReceivingProvider) (not (index $.DialMXSetup $d.ID))}} hidden{{end}}>{{if index $.DomainReceivingRegenerate $d.ID}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/regenerate" data-confirm="Regenerate the Worker secret? The current Worker stops working until you paste the new code."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="amber">Regenerate secret</button></form>{{end}}{{with index $.DialMXSetup $d.ID}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/regenerate" data-antler-regenerate data-confirm="Regenerate the receiving key for {{$d.Name}}? This invalidates the current key and can interrupt incoming mail until you replace the authorization TXT record in DNS and a Tusk receiver reconnects. Your MX records stay the same."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary">Regenerate key</button></form>{{end}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/clear" data-confirm="Remove receiving configuration for this domain? It will stop accepting mail until a receive path is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove receiving</button></form></div><button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="domain-receiving-form-{{$d.ID}}" data-save-provider{{if not $sel}} disabled{{end}}>Save</button></div>
</dialog>
{{end}}
{{range .Domains}}{{$d := .}}<dialog id="domain-catchall-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "catchall")}} data-open="1"{{end}}><h2>Catch-all · {{.Name}}</h2><form method="post" action="/ui/domains/{{.ID}}/catchall"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><p class="muted">Mail sent to an unknown address on this domain is delivered to this inbox. Only inboxes on {{.Name}} can be selected.</p><label>Catch-all inbox</label><select name="inbox"><option value="">No catch-all</option>{{range index $.DomainInboxes $d.ID}}<option value="{{.ID}}"{{if eq .ID $d.CatchAllInboxID}} selected{{end}}>{{.Address}}</option>{{end}}</select><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button>Save catch-all</button></div></form></dialog>
{{end}}
`

const inboxTableTemplate = `{{define "inboxes-table"}}<div class="table-wrap"><table class="dense"><thead><tr><th>Name</th><th>Address</th><th></th><th class="hcenter">Unread</th><th class="hcenter">Pending send</th>{{if $.Principal.Admin}}<th>Connectors</th>{{end}}<th>Size</th>{{if $.Principal.Admin}}<th></th>{{end}}</tr></thead><tbody data-inbox-rows>{{range .Inboxes}}{{$inbox := .}}<tr class="row-link" data-href="/ui/inboxes/{{.ID}}" data-inbox-id="{{.ID}}" data-domain-id="{{.DomainID}}"><td><a href="/ui/inboxes/{{.ID}}">{{if .DisplayName}}{{.DisplayName}}{{else}}<span class="muted">—</span>{{end}}</a></td><td style="white-space:nowrap">{{.Address}}</td><td class="inbox-flags" style="white-space:nowrap">{{if index $.CatchAllInboxes .ID}}{{catchAllIcon}}{{end}}{{$conn := index $.InboxConn .ID}}{{if eq $conn.State "disconnected"}}<a class="issue-dot"{{if eq $conn.Reconnect "one"}} data-one-reconnect data-inbox="{{.ID}}"{{end}} href="/?inbox={{.ID}}&inbox_tab=basic" style="color:#b3261e;vertical-align:middle" title="{{if $conn.Detail}}{{$conn.Detail}}{{else}}Account disconnected — reconnect this inbox{{end}}" aria-label="{{if $conn.Detail}}{{$conn.Detail}}{{else}}Account disconnected — reconnect this inbox{{end}}"><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="8" r="6"/><path d="m5.9 5.9 4.2 4.2M10.1 5.9l-4.2 4.2"/></svg></a>{{end}}{{$sp := index $.InboxSendingReady .ID}}{{$rp := index $.InboxReceivingReady .ID}}{{$recvMissing := "No Receiver Configured for Domain"}}{{if eq .Kind "standalone"}}{{$recvMissing = "No IMAP Connector Configured"}}{{end}}{{if or (not $sp) (not $rp)}}<span class="issue-dot" style="color:#b3261e;vertical-align:middle"{{if and (not $sp) (not $rp)}} title="No Sender Configured for Inbox&#10;{{$recvMissing}}" aria-label="No Sender Configured for Inbox, {{$recvMissing}}"{{else if not $sp}} title="No Sender Configured for Inbox" aria-label="No Sender Configured for Inbox"{{else}} title="{{$recvMissing}}" aria-label="{{$recvMissing}}"{{end}}><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="8" r="6"/><path d="m5.9 5.9 4.2 4.2M10.1 5.9l-4.2 4.2"/></svg></span>{{end}}{{if .SenderRestricted}}<span title="Sender allow list:&#10;{{range $i, $a := .AllowedSenders}}{{if $i}}&#10;{{end}}{{$a}}{{end}}{{if not .AllowedSenders}}None{{end}}&#10;&#10;Matches the From address, which can be spoofed" aria-label="Restricted to allowed senders" style="color:#5f6368;vertical-align:middle"><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="7" width="9" height="6.5" rx="1.2"/><path d="M5.5 7V5.5a2.5 2.5 0 0 1 5 0V7"/></svg></span>{{end}}{{if .Aliases}}<span title="Aliases:&#10;{{range $i, $a := .Aliases}}{{if $i}}&#10;{{end}}{{$a}}{{end}}" aria-label="Has aliases" style="color:#5f6368;vertical-align:middle"><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="6.2" cy="5" r="2.3"/><path d="M10.8 13.4v-1a2.6 2.6 0 0 0-2.6-2.6H4.2a2.6 2.6 0 0 0-2.6 2.6v1"/><path d="M14.2 13.4v-1a2.6 2.6 0 0 0-1.9-2.5"/><path d="M10 2.6a2.3 2.3 0 0 1 0 4.6"/></svg></span>{{end}}</td><td style="white-space:nowrap;text-align:center" data-cell="unread">{{if index $.Unread .ID}}<span class="pill unread-pill">{{index $.Unread .ID}}</span>{{else}}<span class="muted">—</span>{{end}}</td><td style="white-space:nowrap;text-align:center" data-cell="pending">{{if index $.DraftCounts .ID}}<a class="pill pending-pill" href="/ui/inboxes/{{.ID}}/drafts" title="Drafts awaiting approval to send">{{index $.DraftCounts .ID}}</a>{{else}}<span class="muted">—</span>{{end}}</td>{{if $.Principal.Admin}}<td class="inbox-connectors"><div class="connector-chips"><button type="button" class="secondary btn-sm connector-add add-connector" data-inbox="{{$inbox.ID}}" title="Add connector" aria-label="Add connector">+</button>{{range index $.InboxConnectors .ID}}<button type="button" class="secondary btn-sm connector-chip open-inbox-connector" data-inbox="{{$inbox.ID}}" data-connector="{{.ID}}" title="{{if eq .Kind "hermes"}}Hermes Relay: {{.Name}}{{else if eq .Kind "openclaw"}}OpenClaw: {{.Name}}{{else}}Webhook: {{.Name}}&#10;{{shortURL .URL}}{{end}}" aria-label="{{if eq .Kind "hermes"}}Hermes Relay: {{.Name}}{{else if eq .Kind "openclaw"}}OpenClaw: {{.Name}}{{else}}Webhook: {{.Name}}, {{.URL}}{{end}}">{{if eq .Kind "hermes"}}<img class="connector-brand-image" alt="" aria-hidden="true" src="{{asset "hermes-connector.png"}}">{{else if eq .Kind "openclaw"}}<img class="connector-brand-image connector-brand-openclaw" alt="" aria-hidden="true" src="{{asset "openclaw-connector.png"}}">{{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 16.98h-5.99c-1.1 0-1.95.94-2.48 1.9A4 4 0 0 1 2 17c.01-.7.2-1.4.57-2"/><path d="m6 17 3.13-5.78c.53-.97.1-2.18-.5-3.1a4 4 0 1 1 6.89-4.06"/><path d="m12 6 3.13 5.73C15.66 12.7 16.9 13 18 13a4 4 0 0 1 0 8"/></svg>{{end}}</button>{{end}}</div></td>{{end}}<td style="white-space:nowrap"><span class="{{inboxSizeClass $.InboxQuotas $.MailboxSizes .ID}}" title="{{inboxSizeTitle $.InboxQuotas $.MailboxSizes .ID}}">{{filesize (index $.MailboxSizes .ID)}}</span></td>{{if $.Principal.Admin}}<td class="actions" style="white-space:nowrap"><button type="button" class="secondary icon-btn edit-inbox" data-id="{{.ID}}" data-kind="{{.Kind}}" data-name="{{.DisplayName}}" data-address="{{.Address}}" data-remote="{{index $.InboxRemoteConfig .ID}}" data-allowed="{{join .AllowedSenders ","}}" data-restricted="{{if .SenderRestricted}}1{{end}}" data-require-auth="{{if .RequireAuthenticated}}1{{end}}" data-mx="{{if index $.DomainIsMX .DomainID}}1{{end}}" data-approver-email="{{.ApproverEmail}}" data-aliases="{{join .Aliases ","}}" data-alias-names="{{aliasNames .Aliases .AliasNames}}" data-connectors="{{connectorsJSON (index $.InboxConnectors .ID)}}" data-access="{{index $.AccessGrants .ID}}" data-authoring="{{index $.AuthoringSettingsJSON .ID}}" data-default-sender="{{.DefaultSender}}" data-trash-retention="{{if .TrashRetentionDays}}{{.TrashRetentionDays}}{{end}}" data-storage-quota="{{if .StorageQuotaBytes}}{{.StorageQuotaBytes}}{{end}}" data-auto-mark-read="{{if .AutoMarkReadOnDelivery}}1{{end}}" data-auto-trash-hours="{{if .AutoTrashAfterDeliveryHours}}{{.AutoTrashAfterDeliveryHours}}{{end}}" data-delivery-trigger="{{.DeliveryTrigger}}" data-usage="{{filesize (index $.MailboxSizes .ID)}}" title="Inbox settings" aria-label="Inbox settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></button><button type="button" class="secondary icon-btn danger open-delete-inbox" data-id="{{.ID}}" data-address="{{.Address}}" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></td>{{end}}</tr>{{end}}</tbody></table></div>{{end}}`

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin UI requires Admin", 403)
		return
	}
	ctx := r.Context()
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	domains, _ := s.Service.Store.ListDomains(ctx, p.AccountID)
	boxes, _ := s.Service.Store.ListInboxes(ctx, p)
	keys, _ := s.Service.Store.ListAPIKeys(ctx, p.AccountID)
	members, _ := s.Service.Store.ListAccountUsers(ctx, p.AccountID)
	invites, _ := s.Service.Store.ListInvites(ctx, p.AccountID)
	catchAll := make(map[string]string, len(domains))
	catchAllInboxes := make(map[string]bool, len(domains))
	for _, d := range domains {
		if d.CatchAllInboxID != "" {
			catchAll[d.CatchAllInboxID] = d.ID
		}
	}
	for _, b := range boxes {
		if _, isCatchAll := catchAll[b.ID]; isCatchAll && b.Enabled {
			catchAllInboxes[b.ID] = true
		}
	}
	// DomainCatchAllInbox maps a domain to its enabled catch-all inbox, for the
	// key dialog's matrix.
	domainCatchAllInbox := map[string]string{}
	for _, d := range domains {
		if d.CatchAllInboxID != "" && catchAllInboxes[d.CatchAllInboxID] {
			domainCatchAllInbox[d.ID] = d.CatchAllInboxID
		}
	}
	accessGrants := accessGrantsByInbox(boxes, keys, members, invites, catchAll, time.Now().UTC())
	authoringSettings := s.authoringSettingsByInbox(ctx, boxes)
	inboxRemoteConfig := s.inboxRemoteConfigByInbox(ctx, p, boxes)
	conns, _ := s.Service.Store.ListHermesConnections(ctx, p.AccountID)
	webhooks, _ := s.Service.Store.ListWebhookClients(ctx, p.AccountID)
	connectorViews := credentialViews(nil, conns, webhooks)
	inboxConnectors := make(map[string][]credentialView, len(boxes))
	for _, connector := range connectorViews {
		inboxConnectors[connector.InboxID] = append(inboxConnectors[connector.InboxID], connector)
	}
	var msgs []model.Message
	var unlinked []store.DomainLogEntry
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		msgs, _ = s.Service.Store.SearchMessages(ctx, p, q, "", 100)
	} else {
		activity, _ := s.Service.Store.ListAccountLog(ctx, p.AccountID, 100)
		msgs = dashboardActivityRows(activity)
		unlinked = unlinkedDeliveryEvents(activity)
	}
	sendingReady, receivingReady, inboxSendingReady, inboxReceivingReady := inboxReadiness(domains, boxes)
	inboxConn := s.inboxConnectionStates(ctx, p, boxes)
	standaloneInboxes := make([]model.Inbox, 0)
	for _, b := range boxes {
		if b.Kind == model.InboxKindStandalone {
			standaloneInboxes = append(standaloneInboxes, b)
		}
	}
	dialMXSetup := make(map[string]*dialMXSetupView, len(domains))
	domainIsMX := make(map[string]bool, len(domains))
	domainInboxes := make(map[string][]model.Inbox, len(domains))
	for _, b := range boxes {
		domainInboxes[b.DomainID] = append(domainInboxes[b.DomainID], b)
	}
	openID := strings.TrimSpace(r.URL.Query().Get("domain"))
	openKind := strings.TrimSpace(r.URL.Query().Get("kind"))
	for _, d := range domains {
		// The authenticated-sender requirement applies to both direct-SMTP
		// paths (MX and Dial MX).
		switch normalizeDomainProvider(d.ReceivingProvider) {
		case "mx", "dialmx":
			domainIsMX[d.ID] = true
		}
		// A domain whose *effective* receiving provider is Dial MX gets a setup
		// panel. ReceivingProvider is already the effective provider for an
		// inherited subdomain (the store resolves it when listing domains), so a
		// child that inherits Dial MX owns its own exact key here. A domain
		// merely viewed with a ?provider=dialmx query but whose effective
		// provider is something else must not mint a key.
		if normalizeDomainProvider(d.ReceivingProvider) != "dialmx" {
			continue
		}
		credential, err := s.Service.EnsureDialMXCredential(ctx, p.AccountID, d.ID)
		if err != nil {
			continue
		}
		var urls string
		var cfg store.DomainReceivingConfig
		haveCfg := false
		if resolved, err := s.Service.Store.ResolveDomainReceivingConfig(ctx, p.AccountID, d.ID, "dialmx"); err == nil {
			cfg = resolved
			haveCfg = true
			if values, err := s.Service.DecryptDomainReceivingConfig(cfg); err == nil {
				urls, _ = values["receiver_urls"].(string)
			}
		}
		setup := &dialMXSetupView{
			KeyID:        credential.KeyID,
			PublicKey:    credential.PublicKey,
			TXT:          mxwire.DomainTXT(credential.KeyID, decodePublicKey(credential.PublicKey)),
			ReceiverURLs: urls,
		}
		var view dialMXLiveView
		if haveCfg {
			view = s.dialMXLiveView(d.Name, credential.KeyID, decodePublicKey(credential.PublicKey), cfg)
			setup.Service, setup.ContactEmail, setup.MX = view.Service, view.ContactEmail, view.MX
			setup.Enforcement = view.Enforcement
			setup.Statuses = view.Statuses
		} else {
			setup.Statuses = s.dialMXStatuses(d.Name)
		}
		// A subdomain that inherits its receiving config must not offer to edit
		// settings that belong to the ancestor: the change would either be lost or
		// silently fork the child onto its own config.
		setup.InheritedFrom = d.ReceivingInheritedFrom
		// The published-record checks and the extra-MX verdict are the watcher's
		// shared values, so the dashboard light, the setup wizard and the status
		// dialog all read one picture without a render-time lookup.
		setup.DNS = s.dialMXDNS(d.ID)
		setup.ConnectorsJSON = dialMXConnectorsJSON(setup.MX)
		setup.StatusesJSON = dialMXStatusesJSON(setup.Statuses)
		// The aggregate light folds in the shared "extra MX" verdict, and the same
		// value seeds the status dialog's "additional MX servers" warning so it
		// paints on open rather than waiting for the dialog's own fetch to return.
		// The verdict is read for every configured domain, not only one with a
		// ready receiver: it is a fix prompt, and a red domain still needs to name
		// the records to remove. dialMXHealth ignores it with nothing ready, so a
		// red domain stays red.
		var unexpected []string
		if haveCfg {
			unexpected = s.dialMXExtraMX(d.ID)
		}
		// A light means "here is how mail delivery for this domain stands", so it
		// is only drawn for a domain that actually names something to deliver to.
		// A Dial MX domain whose Tusk receiver set is empty — a Tusk setup that
		// was started and abandoned, or a manifest that advertised no receivers —
		// has nothing to deliver to and nothing to fix, so it gets no dot rather
		// than a red "mail delivery is blocked" for a setup that was never finished.
		// This mirrors the MX check and the extra-MX verdict above, both of which
		// also require a configured receiver target.
		if haveCfg && s.dialMXHasReceiverTargets(ctx, p.AccountID, d.ID) {
			setup.Light, setup.LightTitle = dialMXHealth(setup.Statuses, unexpected, time.Now())
		}
		// Seed the status view with the same picture the dashboard light was drawn
		// from, so its fix prompts and extra-MX warning paint on open instead of
		// waiting for the first fetch. The instructions carry only public records.
		setup.InstructionsJSON = dialMXInstructionsJSON(view.instructionView())
		setup.UnexpectedJSON = dialMXUnexpectedJSON(unexpected)
		setup.DNSJSON = dialMXDNSJSON(setup.DNS)
		setup.ManifestJSON = antlerManifestJSON(view.Manifest)
		dialMXSetup[d.ID] = setup
	}
	// A root domain may have had an ancestor added after it, so offer "Inherited
	// (from parent)" in its provider menus. A domain that already has a parent
	// uses its own ParentDomain for that label instead.
	domainParentCandidate := map[string]string{}
	for _, d := range domains {
		if d.ParentDomainID != "" {
			continue
		}
		if candidates := inheritableAncestors(d.Name, domains); len(candidates) > 0 {
			domainParentCandidate[d.ID] = candidates[0].Name
		}
	}
	unread := s.inboxUnreadCounts(ctx, p)
	draftCounts, _ := s.Service.Store.PendingDraftCountsByInbox(ctx, p)
	if draftCounts == nil {
		draftCounts = map[string]int{}
	}
	mailboxSizes, _ := s.Service.Store.MessageSizesByInbox(ctx, p)
	if mailboxSizes == nil {
		mailboxSizes = map[string]int64{}
	}
	// The Size column shows total per-inbox usage (messages + drafts +
	// attachments), matching the per-inbox quota it is compared against, so
	// overwrite the messages-only SUM with each inbox's maintained usage.
	inboxQuotas := make(map[string]*int64, len(boxes))
	for _, box := range boxes {
		mailboxSizes[box.ID] = box.StorageUsedBytes
		inboxQuotas[box.ID] = box.StorageQuotaBytes
	}

	// The dialog to open is named in the query so a provider switch or a
	// post-save error can reload the dashboard with the right editor visible. A
	// foreign or stale domain id is dropped rather than trusted.
	if openID != "" {
		if _, err := s.Service.Store.GetDomain(ctx, p.AccountID, openID); err != nil {
			openID, openKind = "", ""
		}
	}
	// Secondary settings flows can return to a specific inbox tab. Opening inbox
	// settings without an explicit tab defaults to Identity; connector and alias
	// flows explicitly return to their respective tabs.
	inboxOpenID := strings.TrimSpace(r.URL.Query().Get("inbox"))
	inboxOpenTab := strings.TrimSpace(r.URL.Query().Get("inbox_tab"))
	inboxOpenAlias := ""
	if inboxOpenTab == "" {
		inboxOpenTab = "basic"
	}
	switch inboxOpenTab {
	case "basic", "allow", "approver", "quota", "aliases", "connectors", "access":
	default:
		inboxOpenTab = "basic"
	}
	if inboxOpenID != "" {
		if _, err := s.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxOpenID); err != nil {
			inboxOpenID, inboxOpenTab = "", ""
		}
	}

	sendingEditors := make(map[string][]*domainEditorView, len(domains))
	sendingMX := make(map[string]bool, len(domains))
	sendingDKIM := make(map[string]bool, len(domains))
	sendingDKIMRevealURL := make(map[string]string, len(domains))
	sendingLight := make(map[string]*domainLight, len(domains))
	receivingEditors := make(map[string][]*domainEditorView, len(domains))
	sendingSelected := make(map[string]string, len(domains))
	receivingSelected := make(map[string]string, len(domains))
	sendingLabel := make(map[string]string, len(domains))
	receivingLabel := make(map[string]string, len(domains))
	receivingRegenerate := make(map[string]bool, len(domains))
	for _, d := range domains {
		sendProvider := normalizeDomainProvider(d.SendingProvider)
		if d.SendingInheritedFrom != "" {
			sendProvider = providerInherited
		}
		if openID == d.ID && openKind == "sending" {
			if qp := normalizeDomainProvider(r.URL.Query().Get("provider")); qp != "" {
				sendProvider = qp
			}
		}
		sendingSelected[d.ID] = sendProvider
		editors := s.domainSendingEditors(ctx, p.AccountID, d.ID, d.Name)
		for _, e := range editors {
			if e.Provider == sendProvider {
				e.Selected = true
				sendingLabel[d.ID] = e.ProviderLabel
			}
			if e.Provider == "mx" {
				sendingMX[d.ID] = true
				if e.DKIM != nil && e.DKIM.HasKey {
					sendingDKIM[d.ID] = true
					sendingDKIMRevealURL[d.ID] = e.DKIM.RevealURL
				}
			}
		}
		if sendingLabel[d.ID] == "" && d.SendingProvider != "" {
			sendingLabel[d.ID] = d.SendingProvider
		}
		sendingEditors[d.ID] = editors

		recvProvider := normalizeDomainProvider(d.ReceivingProvider)
		if d.ReceivingInheritedFrom != "" {
			recvProvider = providerInherited
			if setup := r.URL.Query().Get("provider"); setup == "dialmx" && openID == d.ID {
				recvProvider = "dialmx"
			}
		}
		if openID == d.ID && openKind == "receiving" {
			if qp := normalizeDomainProvider(r.URL.Query().Get("provider")); qp != "" {
				recvProvider = qp
			}
		}
		receivingSelected[d.ID] = recvProvider
		recvEditors := s.domainReceivingEditors(ctx, p.AccountID, d.ID)
		for _, e := range recvEditors {
			if e.Provider == recvProvider {
				e.Selected = true
				setup := dialMXSetup[d.ID]
				receivingLabel[d.ID] = receivingProviderLabel(e.Provider, setup != nil && setup.Service == "custom")
				receivingRegenerate[d.ID] = e.Generated
			}
		}
		if receivingLabel[d.ID] == "" && d.ReceivingProvider != "" {
			receivingLabel[d.ID] = d.ReceivingProvider
		}
		receivingEditors[d.ID] = recvEditors
	}
	// The Sending-column light is the published DKIM record check for a Direct MX
	// domain that signs. It is read from the watcher's shared cached verdict, so
	// no resolver lookup runs on this render. A domain that does not sign (or has
	// no DKIM check yet) contributes no entry and renders no light.
	for _, d := range domains {
		if !sendingDKIM[d.ID] {
			continue
		}
		if light, title := s.dkimSendingLight(d.ID); light != "" {
			sendingLight[d.ID] = &domainLight{Light: light, Title: title}
		}
	}
	// Build the Direct MX status panel once when any domain's receiving editor
	// set offers the admin-only mx provider (which encodes the admin gate). Every
	// Direct MX dialog shows the same installation receiver state.
	var mxSetup *mxSetupView
	for _, editors := range receivingEditors {
		hasMX := false
		for _, e := range editors {
			if e.Provider == "mx" {
				hasMX = true
				break
			}
		}
		if !hasMX {
			continue
		}
		st, err := s.Service.MXReceiverStatus(ctx)
		if err != nil {
			break
		}
		mxSetup = &mxSetupView{
			Configured: st.Configured,
			Mode:       st.Mode,
			State:      st.State,
		}
		if st.Configured {
			mxSetup.TargetHost = s.mxTargetHost(st)
		}
		break
	}

	if p.SystemAdmin && mxSetup != nil {
		settings, err := s.Service.GetMXReceiverSettings(ctx)
		if err != nil {
			s.Log.Error("cannot read MX receiver settings", "error", err)
			http.Error(w, "cannot read MX receiver settings", 500)
			return
		}
		status, err := s.Service.MXReceiverStatus(ctx)
		if err != nil {
			http.Error(w, "cannot read MX receiver status", 500)
			return
		}
		mxSetup.Editors = make(map[string]mxReceiverEditorView)
		for _, d := range domains {
			form := newMXFormView(settings)
			form.Mode = app.MXModeIncluded
			if form.Hostname == "" {
				if publicURL, err := url.Parse(s.Service.Config.ReceiverURL()); err == nil {
					form.Hostname = publicURL.Hostname()
				}
			}
			if d.ID == openID && openKind == "receiving" {
				if f, ok := s.takeMXFormFlash(r, r.URL.Query().Get("_flash"), p.UserID, d.ID); ok {
					applyMXFlash(&form, f)
				} else {
					form.Error = r.URL.Query().Get("error")
				}
			}
			mxSetup.Editors[d.ID] = mxReceiverEditorView{DomainID: d.ID, CSRF: csrf(r), MXForm: form, MXIncludedSupported: status.IncludedSupported}
		}
	}
	// Remote MX: an account-owned single-mode receiver. Build the panel when the
	// account admin's receiving editor set offers the remotemx provider, or when
	// the provider is already configured (so status still renders for a
	// non-admin viewing a domain that uses it). The bearer secret is never read
	// here.
	var remoteMXSetup *remoteMXSetupView
	if p.Admin {
		settings, err := s.Service.GetAccountMXReceiver(ctx, p.AccountID)
		if err != nil {
			s.Log.Error("cannot read Remote MX receiver settings", "error", err)
			http.Error(w, "cannot read Remote MX receiver settings", 500)
			return
		}
		view := &remoteMXSetupView{
			URL:           settings.URL,
			KeyConfigured: settings.KeyConfigured,
			AllowPrivate:  settings.AllowPrivate,
			CA:            settings.CA,
			Revision:      settings.Revision,
			Configured:    settings.URL != "",
			Error:         r.URL.Query().Get("rx_error"),
		}
		if s.Service.RemoteMXRuntime != nil {
			st := s.Service.RemoteMXRuntime.RemoteMXStatus(ctx, p.AccountID)
			view.State, view.Detail = st.State, st.Detail
		} else if view.Configured {
			view.State = app.MXStateDisabled
		}
		remoteMXSetup = view
	}

	notice, secretLabel, secret := r.URL.Query().Get("notice"), "", ""
	dashboardError := r.URL.Query().Get("error")
	credentialTitle, credentialInstructions, credentialWebhookURL, credentialSecret := "", "", "", ""
	// dkimPrivateKey is set only when a reveal flash was just consumed, so the
	// sending dialog can show it once in a pop-up.
	dkimPrivateKey := ""
	// A connector flash is consumed only after the domain set is known, so a
	// stale flash is never applied to the wrong domain.
	if tok := r.URL.Query().Get("_flash"); tok != "" {
		if v, ok := s.peekFlash(r, tok); ok {
			switch f := v.(type) {
			case secretFlash:
				if taken, ok := s.takeFlash(r, tok); ok {
					if tf, ok := taken.(secretFlash); ok {
						notice, secretLabel, secret = tf.Notice, tf.Label, tf.Secret
					}
				}
			case domainWorkerFlash:
				// Cloudflare Worker code is shown on its own one-time setup page
				// (/ui/domains/{id}/receiving/worker), never on the dashboard.
			case dkimKeyFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if taken, ok := s.takeFlash(r, tok); ok {
						if tf, ok := taken.(dkimKeyFlash); ok && tf.PrivateKey == f.PrivateKey && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == f.DomainID {
							dkimPrivateKey = tf.PrivateKey
						}
					}
				}
			case domainCredentialFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if rc, err := s.Service.Store.GetDomainReceivingConfig(ctx, p.AccountID, f.DomainID); err == nil && rc.ID == f.ConfigID && rc.Revision == f.Revision {
						if taken, ok := s.takeFlash(r, tok); ok {
							if tf, ok := taken.(domainCredentialFlash); ok && tf.Secret == f.Secret && tf.ConfigID == f.ConfigID && tf.Revision == f.Revision && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == f.DomainID {
								credentialTitle, credentialInstructions, credentialWebhookURL, credentialSecret = tf.Title, tf.Instructions, tf.WebhookURL, tf.Secret
							}
						}
					}
				}
			case domainNoticeFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if taken, ok := s.takeFlash(r, tok); ok {
						if tf, ok := taken.(domainNoticeFlash); ok && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == f.DomainID && tf.Error == f.Error {
							if tf.Kind == "sending" {
								for _, e := range sendingEditors[tf.DomainID] {
									if e.Provider == tf.Provider {
										e.Error = tf.Error
										overlayValues(e.Values, tf.Values)
										sendingSelected[tf.DomainID] = tf.Provider
									}
								}
							} else if tf.Kind == "receiving" {
								if tf.Provider == "remotemx" && remoteMXSetup != nil && tf.Values != nil {
									remoteMXSetup.URL = tf.Values["rx_url"]
									remoteMXSetup.CA = tf.Values["rx_ca"]
									remoteMXSetup.AllowPrivate = tf.Values["rx_allow_private"] == "1"
								}
								for _, e := range receivingEditors[tf.DomainID] {
									if e.Provider == tf.Provider {
										e.Error = tf.Error
										overlayValues(e.Values, tf.Values)
										receivingSelected[tf.DomainID] = tf.Provider
									}
								}
							}
							openID, openKind = tf.DomainID, tf.Kind
						}
					}
				}
			}
		}
	}

	// External sending aliases have been removed; no alias dialogs are built.

	w.Header().Set("Cache-Control", "no-store")
	pendingSetups, err := s.Service.Store.ListPendingRelaySetups(ctx, p.AccountID)
	if err != nil {
		s.uiError(w, err, http.StatusInternalServerError)
		return
	}
	s.render(w, r, dashboardBody, pageData{Title: "Dashboard", Tab: "home", Page: "dashboard", ShowOneReconnect: true, Principal: p, CSRF: csrf(r), Account: acc, BaseURL: s.Service.Config.BaseURL, Domains: domains, DomainSendingReady: sendingReady, DomainReceivingReady: receivingReady, DomainIsMX: domainIsMX, InboxSendingReady: inboxSendingReady, InboxReceivingReady: inboxReceivingReady, InboxConn: inboxConn, StandaloneInboxes: standaloneInboxes, DomainInboxes: domainInboxes, DomainSendingEditors: sendingEditors, DomainReceivingEditors: receivingEditors, DomainSendingMX: sendingMX, DomainSendingLight: sendingLight, DomainSendingDKIM: sendingDKIM, DomainSendingDKIMRevealURL: sendingDKIMRevealURL, DomainSendingSelected: sendingSelected, DomainReceivingSelected: receivingSelected, DomainSendingLabel: sendingLabel, DomainReceivingLabel: receivingLabel, DomainReceivingRegenerate: receivingRegenerate, DialMXSetup: dialMXSetup, MXSetup: mxSetup, RemoteMXSetup: remoteMXSetup, DomainParentCandidate: domainParentCandidate, DomainOpenID: openID, DomainOpenKind: openKind, DomainCredentialTitle: credentialTitle, DomainCredentialInstructions: credentialInstructions, DomainCredentialWebhookURL: credentialWebhookURL, DomainCredentialSecret: credentialSecret, DKIMPrivateKey: dkimPrivateKey, DomainNamesCSV: domainNamesCSV(domains), Inboxes: boxes, Messages: msgs, UnlinkedDeliveries: unlinked, Credentials: credentialViews(keys, conns, webhooks), InboxConnectors: inboxConnectors, PendingRelaySetups: pendingSetups, AccessGrants: accessGrants, AuthoringSettingsJSON: authoringSettings, InboxRemoteConfig: inboxRemoteConfig, AccessMembersJSON: accessMembersJSON(members), CatchAllInboxes: catchAllInboxes, DomainCatchAllInbox: domainCatchAllInbox, Unread: unread, MailboxSizes: mailboxSizes, InboxQuotas: inboxQuotas, DraftCounts: draftCounts, InboxAddr: inboxAddrMap(boxes), InboxOpenID: inboxOpenID, InboxOpenTab: inboxOpenTab, InboxOpenAlias: inboxOpenAlias, Notice: notice, SecretLabel: secretLabel, Secret: secret, Error: dashboardError})
}

func (s *Server) uiCreateDomain(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	opts := store.DomainCreateOptions{}
	// The inherit checkboxes only appear (and are submitted) when JS detected a
	// subdomain. With no detection marker the server still auto-detects the
	// parent and inherits by default; inherit_controls=1 means the operator saw
	// the choice, so an unticked box is an explicit opt-out.
	if r.Form.Get("inherit_controls") == "1" {
		opts.DisableReceiving = r.Form.Get("inherit_receiving") != "1"
		opts.DisableSending = r.Form.Get("inherit_sending") != "1"
	}
	if d, err := s.Service.Store.CreateDomainWithOptions(r.Context(), p.AccountID, r.Form.Get("name"), opts); err != nil {
		http.Redirect(w, r, "/?error="+url.QueryEscape(createErrorMessage(err)), 303)
		return
	} else if _, err := s.Service.EnsureDomainDKIMKey(r.Context(), p.AccountID, d.ID); err != nil {
		// The domain exists; a failed key mint is not fatal (the startup
		// backfill retries it) and must not fail the create.
		s.Log.Warn("could not generate DKIM key for new domain", "domain_id", d.ID, "error", err)
	}
	http.Redirect(w, r, "/?notice=Domain+created", 303)
}

// createErrorMessage maps a domain/inbox creation error to a user-safe message.
// Deliberately client-side validation errors keep their text; anything that
// looks like an internal engine fault is replaced with a generic message so the
// dashboard never shows database or filesystem detail.
func createErrorMessage(err error) string {
	return safeErrorMessage(err, "Could not create it. Please try again.")
}

// safeErrorMessage returns err's text unless it looks like an internal engine
// fault (a SQLite error or a filesystem path error), in which case it returns
// fallback. Validation errors produced deliberately keep their message; engine
// faults are redacted so the UI never leaks schema, query state or on-disk
// paths. It mirrors the API's isInternalStoreError policy.
func safeErrorMessage(err error, fallback string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, prefix := range []string{"sqlite error", "sqlite:", "sqlite "} {
		if strings.Contains(msg, prefix) {
			return fallback
		}
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fallback
	}
	return msg
}

// uiError writes a UI error response, redacting engine faults while preserving
// deliberate validation text. Use it wherever a store/service error would
// otherwise be echoed verbatim to the browser.
func (s *Server) uiError(w http.ResponseWriter, err error, code int) {
	if isEngineFault(err) || code >= 500 {
		s.Log.Error("ui handler error", "error", err)
	}
	http.Error(w, safeErrorMessage(err, "Something went wrong. Please try again."), code)
}

// isEngineFault reports whether err looks like an internal engine/filesystem
// fault rather than a deliberate validation error.
func isEngineFault(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, prefix := range []string{"sqlite error", "sqlite:", "sqlite "} {
		if strings.Contains(msg, prefix) {
			return true
		}
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}

// domainNamesCSV joins the account's domain names for the Add Domain dialog's
// client-side subdomain detection. Names never contain a comma.
func domainNamesCSV(domains []model.Domain) string {
	names := make([]string, 0, len(domains))
	for _, d := range domains {
		names = append(names, d.Name)
	}
	return strings.Join(names, ",")
}

// inheritableAncestors returns the domains in the account whose name is a
// proper label-suffix ancestor of name (for example example.com for
// agent.example.com), nearest first. It mirrors the store's parent detection so
// the dashboard can offer a manual link for a root domain whose parent was added
// after it.
func inheritableAncestors(name string, domains []model.Domain) []model.Domain {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	labels := strings.Split(name, ".")
	out := []model.Domain{}
	for i := 1; i < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")
		for _, d := range domains {
			if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d.Name)), ".") == candidate {
				out = append(out, d)
			}
		}
	}
	return out
}

func (s *Server) uiDeleteDomain(w http.ResponseWriter, r *http.Request) {
	if s.Service.MXRuntime != nil {
		defer s.Service.MXRuntime.Wake()
	}
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	dom, err := s.Service.Store.GetDomain(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if !typedConfirmMatches(r.FormValue("confirm"), dom.Name) {
		http.Redirect(w, r, "/?error="+url.QueryEscape("Type the domain name exactly to confirm deletion."), 303)
		return
	}
	paths, err := s.Service.Store.PurgeDomain(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	http.Redirect(w, r, "/?notice=Domain+deleted", 303)
}

// typedConfirmMatches reports whether the caller's typed confirmation matches
// the object's name, ignoring case and surrounding whitespace. It is the
// server-side half of the "type the name to delete" guard; the browser check is
// only a convenience and a script can bypass it.
func typedConfirmMatches(confirm, expected string) bool {
	return strings.EqualFold(strings.TrimSpace(confirm), strings.TrimSpace(expected))
}

// clientConfirmName returns the string a user must retype to delete a client:
// its name, or its type label when the name is empty. The client is looked up
// across API keys, relay connectors (Hermes/OpenClaw) and webhooks.
func (s *Server) clientConfirmName(ctx context.Context, accountID, id string) (string, bool) {
	keys, _ := s.Service.Store.ListAPIKeys(ctx, accountID)
	conns, _ := s.Service.Store.ListHermesConnections(ctx, accountID)
	webhooks, _ := s.Service.Store.ListWebhookClients(ctx, accountID)
	for _, v := range credentialViews(keys, conns, webhooks) {
		if v.ID == id {
			if strings.TrimSpace(v.Name) != "" {
				return v.Name, true
			}
			return v.Type, true
		}
	}
	return "", false
}

// clientDeleteError redirects back to the dashboard with an error notice, or to
// the inbox Connectors sub-view when the delete was posted from there.
func clientDeleteError(w http.ResponseWriter, r *http.Request, msg string) {
	dest := "/?error=" + url.QueryEscape(msg)
	if inboxID := strings.TrimSpace(r.Form.Get("inbox")); inboxID != "" {
		dest += "&inbox=" + url.QueryEscape(inboxID) + "&inbox_tab=connectors"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
func (s *Server) uiCreateInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	cfg, err := parseInboxConfig(r)
	if err != nil {
		http.Redirect(w, r, "/?error="+url.QueryEscape(err.Error()), 303)
		return
	}
	inbox, err := s.Service.Store.CreateInbox(r.Context(), p.AccountID, r.Form.Get("domain"), r.Form.Get("local"), r.Form.Get("display"))
	if err != nil {
		http.Redirect(w, r, "/?error="+url.QueryEscape(createErrorMessage(err)), 303)
		return
	}
	if err = s.applyInboxConfig(r.Context(), p, inbox.ID, cfg); err != nil {
		http.Redirect(w, r, "/?error="+url.QueryEscape(createErrorMessage(err)), 303)
		return
	}
	http.Redirect(w, r, "/?notice=Inbox+created", 303)
}

// normalizeAllowedSenders trims, lowercases and validates allowed-sender
// patterns. Empty entries are dropped; an empty result means "allow all".
func normalizeAllowedSenders(raw []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range raw {
		value, err := model.NormalizeAllowedSender(entry)
		if err != nil {
			return nil, err
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) > 500 {
			return nil, fmt.Errorf("too many allowed senders")
		}
	}
	return out, nil
}

// normalizeApproverEmail validates an optional inbox approver address. An empty
// value clears the approver. Wildcard patterns are not allowed: the approver is
// a single person.
func normalizeApproverEmail(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "*@") || strings.Contains(value, "*") {
		return "", fmt.Errorf("approver must be a single email address")
	}
	normalized, err := model.NormalizeAllowedSender(value)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

// parseAllowedSenders reads the repeated `allowed` form fields.
func parseAllowedSenders(r *http.Request) ([]string, error) {
	return normalizeAllowedSenders(r.Form["allowed"])
}

// aliasForm is one submitted alias before its domain name is resolved to an
// account-owned domain id. DisplayName is the optional sender display name.
type aliasForm struct {
	LocalPart   string
	DomainName  string
	DisplayName string
}

// parseAliasInputs reads the repeated `alias` form fields, each a full
// `local@domain` address, plus a parallel repeated `alias_name` field carrying
// the optional sender display name at the same index.
func parseAliasInputs(r *http.Request) ([]aliasForm, error) {
	names := r.Form["alias_name"]
	forms, err := normalizeAliasForms(r.Form["alias"])
	if err != nil {
		return nil, err
	}
	if len(names) > 0 && len(names) != len(r.Form["alias"]) {
		return nil, fmt.Errorf("alias and alias_name fields do not match")
	}
	for i := range forms {
		if i < len(names) {
			name, err := store.NormalizeAliasDisplayName(names[i])
			if err != nil {
				return nil, err
			}
			forms[i].DisplayName = name
		}
	}
	return forms, nil
}

// normalizeAliasForms lowercases, de-duplicates and range checks submitted
// `local@domain` addresses; domain ownership is resolved later against the
// account. Sender display names are carried through unchanged for validation
// against the store's rules.
func normalizeAliasForms(raw []string) ([]aliasForm, error) {
	if len(raw) > maxInboxAliasesForm {
		return nil, fmt.Errorf("too many aliases")
	}
	seen := map[string]bool{}
	out := make([]aliasForm, 0, len(raw))
	for _, entry := range raw {
		value := strings.ToLower(strings.TrimSpace(entry))
		if value == "" {
			continue
		}
		at := strings.LastIndex(value, "@")
		if at <= 0 || at == len(value)-1 {
			return nil, fmt.Errorf("invalid alias address %q", entry)
		}
		local, domain := value[:at], value[at+1:]
		if strings.ContainsAny(local, "@ <>\t\r\n") || strings.ContainsAny(domain, "@ <>\t\r\n") {
			return nil, fmt.Errorf("invalid alias address %q", entry)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, aliasForm{LocalPart: local, DomainName: domain})
	}
	return out, nil
}

const maxInboxAliasesForm = 100

// inboxConfig holds the per-inbox settings shared by the create and edit
// flows so both validate and persist them identically. External sending aliases
// are intentionally absent: they are stable-id objects managed only through the
// admin endpoints, so an inbox save can never replace (and thus drop the
// connector of) an existing external alias.
type inboxConfig struct {
	allowedSenders       []string
	approverEmail        string
	senderRestricted     bool
	requireAuthenticated bool
	aliases              []aliasForm
	defaultSender        string
	// trashRetention is the inbox's Trash auto-purge override. Nil leaves the
	// override unset (inherit the account default); otherwise it points at the
	// number of days (0 = keep until purged by hand).
	trashRetention *int
	// Delivery-triggered auto-actions for agent/relay connectors, entered on the
	// inbox Connectors tab. autoTrashHours is nil when the sweep is disabled.
	//
	// The Connectors tab is a separate form (its own save endpoint), so its
	// fields are absent from an ordinary inbox save. That absence must mean
	// "leave the policy alone", never "off": autoActionsGiven records whether
	// the submitted form actually carried the auto-action controls, and only
	// then is the policy rewritten (see applyInboxConfig).
	autoActionsGiven       bool
	autoMarkReadOnDelivery bool
	autoTrashHours         *int
	deliveryTrigger        string
	// storageQuota is the inbox's storage cap in bytes, entered on the Quota
	// tab. Nil means the inbox is uncapped (only the account quota applies); a
	// pointer to 0 is treated the same; a positive value is the cap.
	storageQuota *int64
	// remotePollSeconds/remoteFullSyncMinutes are a standalone inbox's sync
	// cadence overrides, entered on the Identity tab's remote section. Nil
	// leaves the override unset (inherit the process default). They are only
	// meaningful for a standalone inbox.
	remotePollSeconds     *int
	remoteFullSyncMinutes *int
	// keyRoles holds staged Clients & Access role changes for API keys that
	// already have (or will lose) access to this inbox, keyed by client id. A
	// value is one of read/assistant/owner; an empty value removes this inbox's
	// binding. Only keys the admin touched appear, so untouched keys are left
	// alone. Parsed from access_role_<keyID> hidden inputs.
	keyRoles map[string]string
	// keyDomainSend holds staged domain-wide sending flag changes, keyed by client
	// id, for the catch-all inbox only. A present key is authoritative (true sets
	// the grant and forces owner, false clears it); an absent key keeps whatever
	// the binding already has. Parsed from access_domain_send_<keyID> inputs.
	keyDomainSend map[string]bool
	// keyDomainSendTouched records which keys appeared in keyDomainSend, so an
	// explicit false is distinguishable from "not present".
	keyDomainSendTouched map[string]bool
}

func parseInboxConfig(r *http.Request) (inboxConfig, error) {
	senders, err := parseAllowedSenders(r)
	if err != nil {
		return inboxConfig{}, err
	}
	approverEmail, err := normalizeApproverEmail(r.Form.Get("approver_email"))
	if err != nil {
		return inboxConfig{}, err
	}
	aliases, err := parseAliasInputs(r)
	if err != nil {
		return inboxConfig{}, err
	}
	var trashRetention *int
	if r.Form.Get("trash_retention_override") == "1" {
		days, derr := strconv.Atoi(strings.TrimSpace(r.Form.Get("trash_retention_days")))
		if derr != nil || days < 0 {
			return inboxConfig{}, fmt.Errorf("trash retention must be a whole number of days (0 or more)")
		}
		trashRetention = &days
	}
	autoMarkRead := r.Form.Get("auto_mark_read_on_delivery") == "1"
	autoActionsGiven := formHasAutoActions(r)
	var autoTrashHours *int
	if r.Form.Get("auto_trash_after_delivery") == "1" {
		hours, herr := strconv.Atoi(strings.TrimSpace(r.Form.Get("auto_trash_after_delivery_hours")))
		if herr != nil || hours <= 0 {
			return inboxConfig{}, fmt.Errorf("auto-trash after delivery must be a whole number of hours (1 or more)")
		}
		autoTrashHours = &hours
	}
	trigger := strings.TrimSpace(r.Form.Get("delivery_trigger"))
	if trigger == "" {
		trigger = store.DeliveryTriggerDefault
	}
	if !validDeliveryTriggerValue(trigger) {
		return inboxConfig{}, fmt.Errorf("delivery trigger must be any or all")
	}
	// Storage cap: the "uncapped" checkbox (or a blank value) clears the cap so
	// only the account quota applies; otherwise a non-negative value with a
	// unit sets it (0 = explicitly unlimited, treated as no inbox cap).
	var storageQuota *int64
	if r.Form.Get("quota_unlimited") != "1" && strings.TrimSpace(r.Form.Get("quota_value")) != "" {
		mult, ok := quotaUnitMultiplier(r.Form.Get("quota_unit"))
		if !ok {
			return inboxConfig{}, fmt.Errorf("unknown storage unit")
		}
		value, qerr := strconv.ParseFloat(strings.TrimSpace(r.Form.Get("quota_value")), 64)
		if qerr != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return inboxConfig{}, fmt.Errorf("enter an inbox storage quota of 0 (uncapped) or greater")
		}
		bytes := value * float64(mult)
		if bytes > math.MaxInt64 {
			return inboxConfig{}, fmt.Errorf("inbox storage quota is too large")
		}
		quota := int64(math.Round(bytes))
		storageQuota = &quota
	}
	// Staged Clients & Access role changes: access_role_<keyID> hidden inputs
	// carry the intended role for each touched key. An empty value removes this
	// inbox's binding. Only touched keys are present.
	keyRoles := map[string]string{}
	for name, values := range r.Form {
		if !strings.HasPrefix(name, "access_role_") {
			continue
		}
		keyID := strings.TrimPrefix(name, "access_role_")
		if keyID == "" || len(values) == 0 {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(values[len(values)-1]))
		switch role {
		case "", "read", "assistant", "owner":
			keyRoles[keyID] = role
		default:
			return inboxConfig{}, fmt.Errorf("invalid access role %q", role)
		}
	}
	// Staged domain-wide sending flags: access_domain_send_<keyID> carries "1" or
	// "0". Only touched keys are present, so an untouched key keeps its grant.
	keyDomainSend := map[string]bool{}
	for name, values := range r.Form {
		if !strings.HasPrefix(name, "access_domain_send_") {
			continue
		}
		keyID := strings.TrimPrefix(name, "access_domain_send_")
		if keyID == "" || len(values) == 0 {
			continue
		}
		keyDomainSend[keyID] = values[len(values)-1] == "1"
	}
	keyDomainSendTouched := map[string]bool{}
	for keyID := range keyDomainSend {
		keyDomainSendTouched[keyID] = true
	}
	// Remote sync cadence (standalone Identity tab). Both fields are entered in
	// whole minutes; a blank field inherits the process default. The new-mail poll
	// additionally accepts an explicit 0, meaning "no quick poll": the inbox then
	// relies on push (IMAP IDLE or a Microsoft webhook) and the periodic full sync
	// instead. A non-blank value must be a whole number at or above the minimum
	// (0 for the poll, 1 for the full sync). The poll interval is stored in seconds
	// (the column's unit), so the form's minutes are converted here.
	pollMinutes, rerr := parseOptionalIntFieldAllowZero(r.Form.Get("remote_poll_minutes"), model.RemotePollMinutesMin, "new-mail poll (minutes)")
	if rerr != nil {
		return inboxConfig{}, rerr
	}
	remoteFull, ferr := parseOptionalIntField(r.Form.Get("remote_full_sync_minutes"), model.RemoteFullSyncMinutesMin, "full sync (minutes)")
	if ferr != nil {
		return inboxConfig{}, ferr
	}
	var remotePoll *int
	if pollMinutes != nil {
		seconds := *pollMinutes * 60
		remotePoll = &seconds
	}
	return inboxConfig{
		allowedSenders:         senders,
		approverEmail:          approverEmail,
		senderRestricted:       r.Form.Get("sender_restricted") == "1",
		requireAuthenticated:   r.Form.Get("require_authenticated") == "1",
		aliases:                aliases,
		defaultSender:          strings.ToLower(strings.TrimSpace(r.Form.Get("default_sender"))),
		trashRetention:         trashRetention,
		autoActionsGiven:       autoActionsGiven,
		autoMarkReadOnDelivery: autoMarkRead,
		autoTrashHours:         autoTrashHours,
		deliveryTrigger:        trigger,
		storageQuota:           storageQuota,
		keyRoles:               keyRoles,
		keyDomainSend:          keyDomainSend,
		keyDomainSendTouched:   keyDomainSendTouched,
		remotePollSeconds:      remotePoll,
		remoteFullSyncMinutes:  remoteFull,
	}, nil
}

// parseOptionalIntField parses an optional whole-number form field. A blank
// value yields nil (inherit the default); a non-blank value below min or
// unparseable is rejected with a labelled error.
func parseOptionalIntField(raw string, min int, label string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min {
		return nil, fmt.Errorf("%s must be a whole number of %d or more", label, min)
	}
	return &n, nil
}

// parseOptionalIntFieldAllowZero is parseOptionalIntField that additionally
// accepts an explicit 0 as a meaningful value ("off"), so a field can offer
// "0 = disabled" without weakening its minimum for other values. A blank field
// still inherits (nil); a negative or non-numeric value is rejected.
func parseOptionalIntFieldAllowZero(raw string, min int, label string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || (n != 0 && n < min) {
		return nil, fmt.Errorf("%s must be 0 (off) or a whole number of %d or more", label, min)
	}
	return &n, nil
}

// formHasAutoActions reports whether a submitted inbox form carried the
// delivery auto-action controls at all.
//
// The controls live on the Connectors tab, which is a separate form posting to
// its own endpoint; they are absent from every other tab's save. Reading that
// absence as "unchecked" would clear the policy on an unrelated edit, so the
// submitter checks for the field before applying anything. The trigger select
// is present whenever the controls are, so any one of the three names is
// sufficient evidence.
func formHasAutoActions(r *http.Request) bool {
	for _, name := range []string{"auto_mark_read_on_delivery", "auto_trash_after_delivery", "delivery_trigger"} {
		if _, ok := r.Form[name]; ok {
			return true
		}
	}
	return false
}

// validDeliveryTriggerValue reports whether v is an accepted delivery trigger.
func validDeliveryTriggerValue(v string) bool {
	return v == store.DeliveryTriggerAny || v == store.DeliveryTriggerAll
}

// applyInboxAliases resolves each submitted alias domain to an account-owned
// domain id and replaces the inbox's alias set.
func (s *Server) applyInboxAliases(ctx context.Context, p model.Principal, inboxID string, aliases []aliasForm) error {
	if !p.Admin {
		return store.ErrForbidden
	}
	inputs := make([]store.AliasInput, 0, len(aliases))
	if len(aliases) > 0 {
		domains, err := s.Service.Store.ListDomains(ctx, p.AccountID)
		if err != nil {
			return err
		}
		byName := make(map[string]string, len(domains))
		for _, d := range domains {
			byName[strings.ToLower(d.Name)] = d.ID
		}
		for _, a := range aliases {
			id, ok := byName[strings.ToLower(a.DomainName)]
			if !ok {
				return fmt.Errorf("unknown domain %q", a.DomainName)
			}
			inputs = append(inputs, store.AliasInput{DomainID: id, LocalPart: a.LocalPart, DisplayName: a.DisplayName})
		}
	}
	return s.Service.Store.SetInboxAliases(ctx, p, inboxID, inputs)
}

func (s *Server) applyInboxConfig(ctx context.Context, p model.Principal, inboxID string, cfg inboxConfig) error {
	if !p.Admin {
		return store.ErrForbidden
	}
	accountID := p.AccountID
	// Require-authenticated only applies to direct SMTP (MX) domains, where the
	// edge supplies SPF/DKIM/DMARC evidence. Clearing it here keeps a hidden
	// control's stale value from lingering when a domain's provider changes.
	if cfg.requireAuthenticated {
		box, err := s.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
		if err != nil {
			return err
		}
		if rcDomain, err := s.Service.Store.GetDomain(ctx, accountID, box.DomainID); err != nil || normalizeDomainProvider(rcDomain.ReceivingProvider) != "mx" {
			cfg.requireAuthenticated = false
		}
	}
	if err := s.Service.Store.SetInboxApprover(ctx, accountID, inboxID, cfg.approverEmail); err != nil {
		return err
	}
	if err := s.Service.Store.SetInboxAllowedSenders(ctx, accountID, inboxID, cfg.allowedSenders); err != nil {
		return err
	}
	if err := s.Service.Store.SetInboxSenderRestricted(ctx, accountID, inboxID, cfg.senderRestricted); err != nil {
		return err
	}
	if err := s.Service.Store.SetInboxRequireAuthenticated(ctx, accountID, inboxID, cfg.requireAuthenticated); err != nil {
		return err
	}
	if err := s.Service.Store.SetInboxTrashRetention(ctx, accountID, inboxID, cfg.trashRetention); err != nil {
		return err
	}
	if err := s.Service.Store.SetInboxStorageQuota(ctx, accountID, inboxID, cfg.storageQuota); err != nil {
		return err
	}
	// Remote sync cadence: only a standalone inbox has a remote server, so the
	// controls are ignored for a domain inbox rather than written.
	if box, err := s.Service.Store.GetInboxInternal(ctx, accountID, inboxID); err == nil && box.Kind == model.InboxKindStandalone {
		if err := s.Service.Store.SetInboxRemoteSync(ctx, accountID, inboxID, cfg.remotePollSeconds, cfg.remoteFullSyncMinutes); err != nil {
			return err
		}
	}
	// Delivery-triggered auto-actions. Only applied when the submitted form
	// actually carried the controls: they live on the Connectors tab, which
	// posts to its own endpoint, so a save from any other tab must leave the
	// policy untouched rather than read the missing fields as "off". When the
	// controls are present the form is authoritative, so an unchecked auto-trash
	// box clears the window rather than leaving a stale value.
	if cfg.autoActionsGiven {
		if err := s.Service.Store.SetInboxAutoActions(ctx, accountID, inboxID, &cfg.autoMarkReadOnDelivery, nil, &cfg.deliveryTrigger); err != nil {
			return err
		}
		if cfg.autoTrashHours != nil {
			if err := s.Service.Store.SetInboxAutoActions(ctx, accountID, inboxID, nil, cfg.autoTrashHours, nil); err != nil {
				return err
			}
		} else if err := s.Service.Store.ClearInboxAutoTrash(ctx, accountID, inboxID); err != nil {
			return err
		}
	}
	if err := s.applyInboxAliases(ctx, p, inboxID, cfg.aliases); err != nil {
		return err
	}
	// Staged Clients & Access role changes, applied in the same save. This runs
	// after aliases so a failed inbox edit never half-applies access.
	if err := s.applyInboxKeyRoles(ctx, accountID, inboxID, cfg.keyRoles, cfg.keyDomainSend, cfg.keyDomainSendTouched); err != nil {
		return err
	}
	// Applied after aliases so a just-set alias can be the default sender.
	return s.Service.Store.SetInboxDefaultSender(ctx, accountID, inboxID, cfg.defaultSender)
}

// applyInboxKeyRoles applies staged per-inbox role changes for API keys. Each
// touched key's full role map is rewritten (read-modify-write) so other inbox
// bindings are preserved; an empty role removes this inbox's binding. Account
// Admin keys have implicit access and cannot be edited per inbox.
//
// domainSend/domainSendTouched carry the staged "Domain-wide sending" flag for
// this inbox: a touched key is authoritative (true sets the grant and forces
// owner, false clears it), and an untouched key keeps its existing grant, so a
// role-only save can never silently drop a grant.
func (s *Server) applyInboxKeyRoles(ctx context.Context, accountID, inboxID string, keyRoles map[string]string, domainSend, domainSendTouched map[string]bool) error {
	// A key may be touched only by the domain-send control (the checkbox changed
	// without a role change), so iterate the union of both staged sets.
	touched := make(map[string]bool, len(keyRoles)+len(domainSendTouched))
	for keyID := range keyRoles {
		touched[keyID] = true
	}
	for keyID := range domainSendTouched {
		touched[keyID] = true
	}
	if len(touched) == 0 {
		return nil
	}
	keys, err := s.Service.Store.ListAPIKeys(ctx, accountID)
	if err != nil {
		return err
	}
	byID := make(map[string]model.APIKey, len(keys))
	for _, k := range keys {
		byID[k.ID] = k
	}
	for keyID := range touched {
		key, ok := byID[keyID]
		if !ok {
			return fmt.Errorf("client not found")
		}
		if key.Admin {
			return fmt.Errorf("account Admin keys have implicit access and cannot be edited per inbox")
		}
		roles := map[string]string{}
		for id, existing := range key.Roles {
			roles[id] = existing
		}
		role, roleStaged := keyRoles[keyID]
		if roleStaged {
			if role == "" {
				delete(roles, inboxID)
			} else {
				roles[inboxID] = role
			}
		}
		// Only a touched domain-send control is authoritative. An untouched one
		// keeps the current grant, which replaceBindings re-applies from the
		// existing rows (nil means "leave as-is").
		var grant map[string]bool
		if domainSendTouched[keyID] {
			if domainSend[keyID] {
				// Turning the grant on pins the role to owner: the two are one
				// state, so the binding must exist and must be owner.
				roles[inboxID] = "owner"
			}
			// Turning it off just clears the grant; the store leaves the role as
			// it is (the system never demotes on its own).
			grant = map[string]bool{inboxID: domainSend[keyID]}
		}
		if err := s.Service.Store.UpdateAPIKey(ctx, accountID, keyID, key.Name, false, roles, grant); err != nil {
			return err
		}
		s.Service.Hub.CancelScope("key:" + keyID)
		s.Service.Store.DeleteKeySessionsForClient(ctx, keyID)
	}
	return nil
}

func (s *Server) uiUpdateInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	id := r.PathValue("id")
	inbox, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	cfg, err := parseInboxConfig(r)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	// A standalone inbox's Identity tab also carries its remote IMAP/SMTP
	// connector. Detect a real change and verify it live before persisting
	// anything, so a bad host/password is rejected rather than saved inactive.
	var remoteIn store.StandaloneRemoteUpdate
	remoteChanged := false
	// A provider-connected inbox (Microsoft, Google) has no editable IMAP binding:
	// its Identity tab shows the provider's reconnect link instead of the
	// connector fields, so those fields always arrive blank. A blank form was read
	// as a connector change, which sent a provider inbox's save through the live
	// IMAP verification gate; that gate has no IMAP credentials to test for a
	// provider inbox, so the save bounced with "IMAP connection failed" and
	// persisted nothing — not even the display name.
	if inbox.Kind == model.InboxKindStandalone && !s.providerConnected(r.Context(), p.AccountID, id) {
		remoteIn = remoteUpdateFromForm(r)
		remoteChanged = standaloneRemoteChanged(inbox, remoteIn)
		if remoteChanged {
			if verr := s.verifyRemoteUpdate(r.Context(), p, id, remoteIn); verr != nil {
				http.Redirect(w, r, "/?inbox="+id+"&inbox_tab=basic&error="+url.QueryEscape("IMAP connection failed: "+safeErrorMessage(verr, "the connector could not sign in")), 303)
				return
			}
		}
	}
	if err = s.Service.Store.SetInboxDisplayName(r.Context(), p.AccountID, id, r.Form.Get("display")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	if err = s.applyInboxConfig(r.Context(), p, id, cfg); err != nil {
		s.uiError(w, err, 400)
		return
	}
	// The Identity tab's Dashboard group is a per-user display preference, not
	// inbox policy: it rides this same save so one Save updates both, but it is
	// written against the signed-in user's preferences and only when that user
	// can hold them (session admin). A save from any other principal leaves the
	// inbox policy write intact and simply does not touch the group.
	if r.Form.Has("dashboard_group") && p.Admin && p.ViaSession && p.UserID != "" {
		prefs, perr := s.Service.Store.GetDashboardPreferences(r.Context(), p)
		if perr != nil {
			s.uiError(w, perr, 400)
			return
		}
		if prefs.Groups == nil {
			prefs.Groups = map[string]map[string]string{}
		}
		if prefs.Groups["inboxes"] == nil {
			prefs.Groups["inboxes"] = map[string]string{}
		}
		group := strings.TrimSpace(r.Form.Get("dashboard_group"))
		if len([]rune(group)) > 64 {
			s.uiError(w, fmt.Errorf("dashboard group name is too long"), 400)
			return
		}
		if group == "" {
			delete(prefs.Groups["inboxes"], id)
		} else {
			prefs.Groups["inboxes"][id] = group
		}
		if perr = s.Service.Store.SetDashboardPreferences(r.Context(), p, prefs); perr != nil {
			s.uiError(w, perr, 400)
			return
		}
	}
	if remoteChanged {
		if _, err = s.remoteMailbox().ConfigureStandaloneRemote(r.Context(), p, id, remoteIn); err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	// Approvals tab: the authoring mode is saved with the same form for a
	// standalone inbox. A domain inbox is preset to the Mammoth Mail approval
	// workflow, so its hidden (now invisible) control is ignored rather than
	// applied — a stale value can never flip the preset. An empty mode clears the
	// override to the kind default. Moving a standalone inbox to the remote-draft
	// mode clears any approver: a remote-draft request just places the draft in
	// the connected Drafts folder, so it never involves an approver or an email.
	if inbox.Kind == model.InboxKindStandalone {
		if r.Form.Has("authoring_mode") {
			selected := strings.TrimSpace(r.Form.Get("authoring_mode"))
			if selected != "" && !model.AuthoringModeAllowedForKind(inbox.Kind, selected) {
				s.uiError(w, fmt.Errorf("unknown authoring mode"), 400)
				return
			}
			if err = s.Service.Store.SetInboxAuthoringMode(r.Context(), p, id, selected); err != nil {
				s.uiError(w, err, 400)
				return
			}
			if model.NormalizeAuthoringMode(inbox.Kind, selected) == model.AuthoringRemoteDraft {
				if err = s.Service.Store.SetInboxApprover(r.Context(), p.AccountID, id, ""); err != nil {
					s.uiError(w, err, 400)
					return
				}
			}
		}
	}
	// A successful save closes the settings dialog for both inbox kinds.
	http.Redirect(w, r, "/?notice=Inbox+updated", 303)
}

func (s *Server) uiDeleteInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	inbox, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if !typedConfirmMatches(r.FormValue("confirm"), inbox.Address) {
		http.Redirect(w, r, "/?error="+url.QueryEscape("Type the inbox address exactly to confirm deletion."), 303)
		return
	}
	paths, err := s.Service.Store.PurgeInbox(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	http.Redirect(w, r, "/?notice=Inbox+deleted", 303)
}

// uiInboxAutoActions updates only the delivery-triggered auto-actions on an
// inbox, so the connector settings screen can save them without reposting the
// whole inbox edit form. The form is authoritative: an unchecked auto-trash box
// clears the window.
func (s *Server) uiInboxAutoActions(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	id := r.PathValue("id")
	if _, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id); err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if err := s.applyInboxAutoActionsForm(r, p.AccountID, id); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/?notice=Inbox+auto-actions+saved&inbox="+url.QueryEscape(id)+"&inbox_tab=connectors", 303)
}

// applyInboxAutoActionsForm applies the delivery auto-action controls from a
// form to an inbox. The form is authoritative: an unchecked auto-trash box
// clears the window. It is shared by the dedicated auto-actions route and the
// inbox settings flow, keeping the policy independent of connector lifecycle.
func (s *Server) applyInboxAutoActionsForm(r *http.Request, accountID, inboxID string) error {
	markRead := r.Form.Get("auto_mark_read_on_delivery") == "1"
	trigger := strings.TrimSpace(r.Form.Get("delivery_trigger"))
	if trigger == "" {
		trigger = store.DeliveryTriggerDefault
	}
	if !validDeliveryTriggerValue(trigger) {
		return fmt.Errorf("delivery trigger must be any or all")
	}
	if err := s.Service.Store.SetInboxAutoActions(r.Context(), accountID, inboxID, &markRead, nil, &trigger); err != nil {
		return err
	}
	if r.Form.Get("auto_trash_after_delivery") == "1" {
		hours, herr := strconv.Atoi(strings.TrimSpace(r.Form.Get("auto_trash_after_delivery_hours")))
		if herr != nil || hours <= 0 {
			return fmt.Errorf("auto-trash after delivery must be a whole number of hours (1 or more)")
		}
		return s.Service.Store.SetInboxAutoActions(r.Context(), accountID, inboxID, nil, &hours, nil)
	}
	return s.Service.Store.ClearInboxAutoTrash(r.Context(), accountID, inboxID)
}

func (s *Server) uiCreateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	notice, label, secret := "", "", ""
	if r.Form.Get("type") == "webhook" {
		mode, authMode := r.Form.Get("mode"), r.Form.Get("auth")
		if mode == "" {
			mode = "notify"
		}
		if authMode == "" {
			authMode = "signature"
		}
		if err := validateWebhookConfig(r.Form.Get("url"), mode, authMode); err != nil {
			s.uiError(w, err, 400)
			return
		}
		if err := validateWebhookBearerSecret(authMode, r.Form.Get("bearer_secret")); err != nil {
			s.uiError(w, err, 400)
			return
		}
		plain, err := webhookSecret(r.Form.Get("bearer_secret"))
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		enc, err := s.Service.EncryptSecretAAD(app.WebhookSecretAAD(p.AccountID, r.Form.Get("inbox")), []byte(plain))
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		name := strings.TrimSpace(r.Form.Get("name"))
		if name == "" {
			name = "Webhook"
		}
		if _, err := s.Service.Store.CreateWebhookClient(r.Context(), p.AccountID, r.Form.Get("inbox"), name, strings.TrimSpace(r.Form.Get("url")), mode, authMode, enc); err != nil {
			s.uiError(w, err, 400)
			return
		}
		notice, label, secret = "Webhook created", "Copy this signing secret now — you will only be able to see it now, it will not be shown again.", plain
		if authMode == "bearer" {
			label = "Copy this bearer secret now — it will not be shown again."
		}
	} else if r.Form.Get("type") == "hermes" {
		inboxID := r.Form.Get("inbox")
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, inboxID); err == nil && !box.SenderRestricted && r.Form.Get("ack") != "1" {
			http.Error(w, "confirm the no-allow-list risk before creating a Hermes relay connection to this inbox", 400)
			return
		}
		gatewayID, gwSecret, _, err := s.Service.CreateHermesRelay(r.Context(), p, inboxID, r.Form.Get("name"))
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		notice, label, secret = "Hermes relay connection created", "Add these lines to your Hermes gateway .env, then restart the gateway — the steps below cover it.", hermesEnvBlock(s.Service.Config.BaseURL, gatewayID, gwSecret)
	} else if r.Form.Get("type") == "openclaw" {
		inboxID := r.Form.Get("inbox")
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, inboxID); err == nil && !box.SenderRestricted && r.Form.Get("ack") != "1" {
			http.Error(w, "confirm the no-allow-list risk before creating an OpenClaw connector to this inbox", 400)
			return
		}
		notice, label, secret = s.createOpenClawConnector(w, r, p, inboxID)
		if notice == "" {
			return
		}
	} else {
		boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
		roles := map[string]string{}
		for _, b := range boxes {
			if role := r.Form.Get("role_" + b.ID); role != "" {
				roles[b.ID] = role
			}
		}
		grant := domainSendForm(r, boxes)
		_, plain, err := s.Service.Store.CreateAPIKey(r.Context(), p.AccountID, r.Form.Get("name"), r.Form.Get("admin") == "1", roles, grant)
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		notice, label, secret = "API key created", "Copy this API key now — you will only be able to see this key now, it will not be shown again.", plain
	}
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, map[string]string{"notice": notice, "label": label, "secret": secret})
		return
	}
	s.flashSecret(w, r, notice, label, secret)
}

// domainSendForm reads the key dialog's per-inbox domain-wide sending values
// (domainsend_<inboxID>), which the matrix submits as an explicit 1 or 0 for
// every catch-all row it renders. The control exists only on a domain's enabled
// catch-all row, so the returned map names exactly those inboxes: "1" grants and
// "0" clears. A map that names no inbox is nil, which tells the store to leave
// every existing grant untouched — so an inbox the dialog does not render is
// never wiped by an unrelated save.
func domainSendForm(r *http.Request, boxes []model.Inbox) map[string]bool {
	grant := map[string]bool{}
	for _, b := range boxes {
		key := "domainsend_" + b.ID
		if _, present := r.Form[key]; !present {
			continue
		}
		grant[b.ID] = r.Form.Get(key) == "1"
	}
	if len(grant) == 0 {
		return nil
	}
	return grant
}

// wantsJSON reports whether the caller asked for a JSON response, so the
// Add Client dialog can receive the one-time secret inline without a redirect.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// flashSecret stores a one-time secret and redirects to the dashboard, which
// consumes it (Post/Redirect/Get). If storage is unavailable, it displays the
// secret inline so a successful create/rotation never loses the only copy.
func (s *Server) flashSecret(w http.ResponseWriter, r *http.Request, notice, label, secret string) {
	if tok := s.putFlash(w, r, secretFlash{Notice: notice, Label: label, Secret: secret}, len(notice)+len(label)+len(secret)+64); tok != "" {
		http.Redirect(w, r, "/?_flash="+tok, http.StatusSeeOther)
		return
	}
	// The secret has already been created/rotated and cannot be retrieved again.
	// If flash storage is unavailable, show it in this no-store response rather
	// than redirecting and silently losing the only copy.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	robotsMeta := ""
	if s.Service.Config.NoIndex {
		robotsMeta = `<meta name="robots" content="noindex,nofollow">`
	}
	_ = template.Must(template.New("secret-fallback").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+robotsMeta+`<title>{{.Notice}}</title></head><body><h1>{{.Notice}}</h1><p>{{.Label}}</p><pre style="white-space:pre-wrap;overflow-wrap:anywhere">{{.Secret}}</pre><p><a href="/">Continue</a></p></body></html>`)).Execute(w, secretFlash{Notice: notice, Label: label, Secret: secret})
}

func (s *Server) uiUpdateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
	roles := map[string]string{}
	for _, b := range boxes {
		if role := r.Form.Get("role_" + b.ID); role != "" {
			roles[b.ID] = role
		}
	}
	grant := domainSendForm(r, boxes)
	if err := s.Service.Store.UpdateAPIKey(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("name"), r.Form.Get("admin") == "1", roles, grant); err != nil {
		s.uiError(w, err, 400)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
	s.Service.Store.DeleteKeySessionsForClient(r.Context(), r.PathValue("id"))
	http.Redirect(w, r, "/?notice=Key+updated", 303)
}

func (s *Server) uiRotateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	plain, err := s.Service.Store.RotateAPIKey(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
	s.Service.Store.DeleteKeySessionsForClient(r.Context(), r.PathValue("id"))
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]string{"notice": "API key rotated", "label": "Copy this API key now — you will only be able to see this key now, it will not be shown again.", "secret": plain})
		return
	}
	s.flashSecret(w, r, "API key rotated", "Copy this API key now — you will only be able to see this key now, it will not be shown again.", plain)
}

func (s *Server) uiDeleteKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	name, ok := s.clientConfirmName(r.Context(), p.AccountID, r.PathValue("id"))
	if !ok {
		http.Error(w, "client not found", 404)
		return
	}
	if !typedConfirmMatches(r.FormValue("confirm"), name) {
		clientDeleteError(w, r, "Type the client name exactly to confirm deletion.")
		return
	}
	if err := s.Service.Store.RevokeAPIKey(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
	s.Service.Store.DeleteKeySessionsForClient(r.Context(), r.PathValue("id"))
	http.Redirect(w, r, "/?notice=Key+deleted", 303)
}

func connectorSettingsRedirect(w http.ResponseWriter, r *http.Request, notice string) {
	dest := "/?notice=" + url.QueryEscape(notice)
	if inboxID := strings.TrimSpace(r.Form.Get("inbox")); inboxID != "" {
		dest += "&inbox=" + url.QueryEscape(inboxID) + "&inbox_tab=connectors"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) uiUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := validateWebhookConfig(r.Form.Get("url"), r.Form.Get("mode"), r.Form.Get("auth")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" {
		name = "Webhook"
	}
	if err := validateWebhookBearerSecret(r.Form.Get("auth"), r.Form.Get("bearer_secret")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	hook, err := s.Service.Store.GetWebhookClient(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	encrypted := ""
	if secret := r.Form.Get("bearer_secret"); secret != "" {
		var err error
		encrypted, err = s.Service.EncryptSecretAAD(app.WebhookSecretAAD(hook.AccountID, hook.InboxID), []byte(secret))
		if err != nil {
			http.Error(w, "webhook client update failed", 500)
			return
		}
	}
	if err := s.Service.Store.UpdateWebhookClientWithSecret(r.Context(), p.AccountID, r.PathValue("id"), name, strings.TrimSpace(r.Form.Get("url")), r.Form.Get("mode"), r.Form.Get("auth"), encrypted); err != nil {
		s.uiError(w, err, 400)
		return
	}
	connectorSettingsRedirect(w, r, "Webhook updated")
}

func (s *Server) uiRotateWebhook(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	hook, err := s.Service.Store.GetWebhookClient(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	plain, err := auth.RandomToken(32)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	enc, err := s.Service.EncryptSecretAAD(app.WebhookSecretAAD(hook.AccountID, hook.InboxID), []byte(plain))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if err := s.Service.Store.RotateWebhookSecret(r.Context(), p.AccountID, r.PathValue("id"), enc); err != nil {
		s.uiError(w, err, 400)
		return
	}
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]string{"notice": "Webhook secret rotated", "label": "Copy this signing secret now — you will only be able to see it now, it will not be shown again.", "secret": plain})
		return
	}
	s.flashSecret(w, r, "Webhook secret rotated", "Copy this signing secret now — you will only be able to see it now, it will not be shown again.", plain)
}

// clientDeliveries renders the per-client delivery log behind the Clients
// table "Log" button for a Webhook or Hermes relay. It shows the event, the
// transport outcome (queued, delivered, acknowledged, skipped or failed), the
// attempt count and the last error, newest event first.
func (s *Server) clientDeliveries(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	keys, _ := s.Service.Store.ListAPIKeys(ctx, p.AccountID)
	conns, _ := s.Service.Store.ListHermesConnections(ctx, p.AccountID)
	webhooks, _ := s.Service.Store.ListWebhookClients(ctx, p.AccountID)
	var client *credentialView
	for _, v := range credentialViews(keys, conns, webhooks) {
		if v.ID == id && (v.Kind == "webhook" || v.Kind == "hermes" || v.Kind == "openclaw") {
			c := v
			client = &c
			break
		}
	}
	if client == nil {
		http.Error(w, "client not found", 404)
		return
	}
	beforeID := int64(1 << 62)
	if v := strings.TrimSpace(r.URL.Query().Get("before")); v != "" {
		if n, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil && n > 0 {
			beforeID = n
		}
	}
	entries, err := s.Service.Store.ClientDeliveryLog(ctx, p.AccountID, id, 51, beforeID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	hasMore := len(entries) > 50
	if hasMore {
		entries = entries[:50]
	}
	nextBefore := int64(0)
	if len(entries) > 0 {
		nextBefore = entries[len(entries)-1].EventID
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, clientDeliveriesBody, pageData{
		Title:            client.Name + " · Log",
		Tab:              "home",
		Principal:        p,
		CSRF:             csrf(r),
		Account:          acc,
		ClientLogClient:  client,
		ClientLogEntries: entries,
		ClientLogHasMore: hasMore,
		ClientLogBefore:  nextBefore,
	})
}

func (s *Server) uiToggleWebhook(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.SetWebhookEnabled(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("enabled") == "1"); err != nil {
		s.uiError(w, err, 400)
		return
	}
	connectorSettingsRedirect(w, r, "Webhook updated")
}

func (s *Server) uiDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	name, ok := s.clientConfirmName(r.Context(), p.AccountID, r.PathValue("id"))
	if !ok {
		http.Error(w, "client not found", 404)
		return
	}
	if !typedConfirmMatches(r.FormValue("confirm"), name) {
		clientDeleteError(w, r, "Type the client name exactly to confirm deletion.")
		return
	}
	if err := s.Service.Store.DeleteWebhookClient(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	connectorSettingsRedirect(w, r, "Webhook deleted")
}

func (s *Server) uiUpdateHermes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.UpdateHermesConnectionName(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("name")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	if role := r.Form.Get("role"); role != "" {
		if err := s.Service.Store.SetHermesOutboundRole(r.Context(), p.AccountID, r.PathValue("id"), role); err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	connectorSettingsRedirect(w, r, "Connection updated")
}

func (s *Server) uiDeleteHermes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	name, ok := s.clientConfirmName(r.Context(), p.AccountID, r.PathValue("id"))
	if !ok {
		http.Error(w, "client not found", 404)
		return
	}
	if !typedConfirmMatches(r.FormValue("confirm"), name) {
		clientDeleteError(w, r, "Type the client name exactly to confirm deletion.")
		return
	}
	if err := s.Service.Store.DeleteHermesConnection(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	s.Service.Hub.CancelScope("hrm:" + r.PathValue("id"))
	connectorSettingsRedirect(w, r, "Connection deleted")
}

func (s *Server) uiCancelRelaySetup(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}
	if err := s.Service.Store.CancelRelaySetup(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		mapStoreError(w, err)
		return
	}
	connectorSettingsRedirect(w, r, "Setup cancelled")
}

// cloudflareWorkerCode renders the embedded Worker template with the given
// base URL and generated shared secret.
func (s *Server) cloudflareWorkerCode(baseURL, secret string) string {
	code := string(cloudflareWorkerTemplate)
	code = strings.ReplaceAll(code, "__MAIL_WEBHOOK_URL__", strings.TrimRight(baseURL, "/")+"/internal/ingest/cloudflare")
	code = strings.ReplaceAll(code, "__MAIL_WEBHOOK_SECRET__", secret)
	return code
}

// deliveryStatus renders the transport-neutral delivery-log status for an event
// as an operator-facing label. A pending row is "Retrying" once an attempt has
// failed and a retry is scheduled, and "Queued" while it has not been attempted.
func deliveryStatus(status string, attempts int, lastError string) string {
	switch status {
	case "delivered":
		return "Delivered"
	case "acknowledged":
		return "Acknowledged"
	case "skipped":
		return "Skipped"
	case "failed":
		return "Failed"
	default:
		if attempts > 0 || lastError != "" {
			return "Retrying"
		}
		return "Queued"
	}
}

// credentialViews combines API keys, Hermes relay connections and webhook
// delivery clients into the dashboard "Clients" list.
func credentialViews(keys []model.APIKey, conns []store.HermesConnection, webhooks []store.WebhookClient) []credentialView {
	out := make([]credentialView, 0, len(keys)+len(conns)+len(webhooks))
	for _, k := range keys {
		v := credentialView{ID: k.ID, Kind: "api", Name: k.Name, Type: "API key", Scope: apiKeyScope(k), Admin: k.Admin}
		if len(k.Roles) > 0 {
			if b, err := json.Marshal(k.Roles); err == nil {
				v.RolesJSON = string(b)
			}
		}
		// Only live grants (the inbox is still the domain's enabled catch-all) are
		// reported, because only those are the ones the checkbox can truthfully
		// show as on.
		var granted []string
		for inboxID, live := range k.DomainSend {
			if live {
				granted = append(granted, inboxID)
			}
		}
		if len(granted) > 0 {
			sort.Strings(granted)
			if b, err := json.Marshal(granted); err == nil {
				v.DomainSendJSON = string(b)
			}
		}
		out = append(out, v)
	}
	for _, h := range conns {
		role := h.OutboundRole
		if role == "" {
			role = "owner"
		}
		scope := "Owner"
		if role == "assistant" {
			scope = "Assistant"
		}
		kind := h.Kind
		typ := "Hermes relay"
		if kind == string(store.KindOpenClaw) {
			typ = "OpenClaw connector"
		} else {
			kind = "hermes"
		}
		out = append(out, credentialView{ID: h.ID, Kind: kind, Name: h.Name, Type: typ, Scope: scope, InboxID: h.InboxID, Role: role, Enabled: true, LastConnectedAt: h.LastConnectedAt})
	}
	for _, wh := range webhooks {
		state := "Active"
		if !wh.Enabled {
			state = "Paused"
		}
		out = append(out, credentialView{ID: wh.ID, Kind: "webhook", Name: wh.Name, Type: "Webhook", Scope: state, InboxID: wh.InboxID, Role: wh.Mode, URL: wh.URL, Mode: wh.Mode, AuthMode: wh.AuthMode, Enabled: wh.Enabled})
	}
	return out
}

func apiKeyScope(k model.APIKey) string {
	if k.Admin {
		return "Admin"
	}
	if len(k.Roles) == 0 {
		return "None"
	}
	seen := map[string]bool{}
	roles := make([]string, 0, len(k.Roles))
	for _, role := range k.Roles {
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	sort.Slice(roles, func(i, j int) bool { return roleRank(roles[i]) < roleRank(roles[j]) })
	for i, role := range roles {
		roles[i] = titleRole(role)
	}
	return strings.Join(roles, ", ")
}

func roleRank(role string) int {
	switch role {
	case "owner":
		return 0
	case "assistant":
		return 1
	case "read":
		return 2
	default:
		return 3
	}
}

func titleRole(role string) string {
	if role == "" {
		return role
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

func hermesEnvBlock(baseURL, gatewayID, secret string) string {
	return fmt.Sprintf("GATEWAY_RELAY_URL=%s\nGATEWAY_RELAY_ID=%s\nGATEWAY_RELAY_SECRET=%s\nGATEWAY_RELAY_PLATFORMS=email\nGATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true",
		baseURL, gatewayID, secret)
}

// createOpenClawConnector creates an OpenClaw relay connector (direct
// credentials) or mints a one-time setup code, depending on the setup method
// selected in the connector dialog. It returns the one-time notice, label and
// secret to show; on error it writes the response itself and returns empty
// strings.
func (s *Server) createOpenClawConnector(w http.ResponseWriter, r *http.Request, p model.Principal, inboxID string) (notice, label, secret string) {
	name := r.Form.Get("name")
	if r.Form.Get("setup") == "manual" {
		gatewayID, gwSecret, _, err := s.Service.CreateRelay(r.Context(), p, inboxID, name, store.KindOpenClaw)
		if err != nil {
			s.uiError(w, err, 400)
			return "", "", ""
		}
		return "OpenClaw connector created",
			"Paste this config block into the OpenClaw host (~/.openclaw/openclaw.json), then restart the Gateway. The secret is shown only once.",
			openClawConfigBlock(s.Service.Config.BaseURL, gatewayID, gwSecret)
	}
	code, err := s.Service.CreateRelayEnrollCode(r.Context(), p, inboxID, name, store.KindOpenClaw, 15*time.Minute)
	if err != nil {
		s.uiError(w, err, 400)
		return "", "", ""
	}
	base := strings.TrimRight(s.Service.Config.BaseURL, "/")
	return "OpenClaw setup code created",
		"Run this on the OpenClaw host (the code expires in 15 minutes):",
		"openclaw channels add --channel mammothmail --code " + base + "/#" + code
}

// openClawConfigBlock is the manual fallback for air-gapped installs: a
// channels.mammothmail JSON5 block carrying the minted credentials.
func openClawConfigBlock(baseURL, gatewayID, secret string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return fmt.Sprintf(`{
  "channels": {
    "mammothmail": {
      "enabled": true,
      "baseUrl": %q,
      "gatewayId": %q,
      "secret": %q
    }
  }
}`, baseURL, gatewayID, secret)
}

const messageBody = `<style>.mailframe{flex:0 0 auto;height:360px}</style><div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{if .StandaloneMode}}{{if .StandaloneProvider}}{{else if not .StandaloneConfigured}}<div class="banner warn">This standalone inbox has no remote connector configured yet. <a href="{{.RemoteConnectorURL}}">Set up IMAP/SMTP</a>.</div>{{else if .StandalonePlain}}<div class="banner warn">This inbox connects to its remote server over plaintext (no transport security). <a href="{{.RemoteConnectorURL}}">Review connection settings</a>.</div>{{end}}{{else}}{{if not .OutboundReady}}<div class="banner warn">{{if .SendingPausedExternal}}Sending paused — configure the sending connector for the selected sender ({{.SendingPausedAddress}}). Mail will queue. <a href="{{.SendingPausedURL}}">Configure</a>.{{else}}Sending is paused until a provider is configured for this domain. Mail will queue. <a href="{{.DomainSendingSettingsURL}}">Add one</a>.{{end}}</div>{{end}}
{{if not .InboundReady}}<div class="banner warn">Not receiving — no receive path is configured for this domain. <a href="{{.DomainReceivingSettingsURL}}">Add one</a>.</div>{{end}}{{end}}` + reconnectBanner + `
 <section class="card mail-reader"><div class="msghead"><h1>{{if .Message.Subject}}{{.Message.Subject}}{{else}}(no subject){{end}}</h1><div class="actions">{{if .Message.DeletedAt}}<form method="post" action="/ui/messages/{{.Message.ID}}/restore"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary icon-btn" title="Restore" aria-label="Restore">` + iconRestore + `</button></form><form method="post" action="/ui/messages/{{.Message.ID}}/purge" data-confirm="Delete this message permanently? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary icon-btn danger" title="Delete forever" aria-label="Delete forever">` + iconDeleteFore + `</button></form>{{else}}<a class="btn secondary icon-btn" title="Reply" aria-label="Reply" href="/ui/messages/{{.Message.ID}}/reply">` + iconReply + `</a><a class="btn secondary icon-btn" title="Reply all" aria-label="Reply all" href="/ui/messages/{{.Message.ID}}/reply-all">` + iconReplyAll + `</a><a class="btn secondary icon-btn" title="Forward" aria-label="Forward" href="/ui/messages/{{.Message.ID}}/forward">` + iconForward + `</a><form method="post" action="/ui/messages/{{.Message.ID}}/read"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="read" value="0"><button class="secondary icon-btn" title="Mark unread" aria-label="Mark unread">` + iconMarkUnread + `</button></form><form method="post" action="/ui/messages/{{.Message.ID}}/delete"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary icon-btn danger" title="Move to trash" aria-label="Move to trash">` + iconTrash + `</button></form>{{end}}</div></div>
<div class="msgmeta"><p class="muted"><b>From:</b> {{if .Message.From.Name}}{{.Message.From.Name}} &lt;{{.Message.From.Address}}&gt;{{else}}{{.Message.From.Address}}{{end}}<br><b>To:</b> {{join .Message.To ", "}}{{if .Message.CC}}<br><b>Cc:</b> {{join .Message.CC ", "}}{{end}}<br><b>Date:</b> {{localDateTime .Message.CreatedAt}}{{if .Inbox}} · <b>Mailbox:</b> {{.Inbox.Address}}{{end}}</p>{{if and .Capabilities.Labels (.Principal.CanAssist .Inbox.ID)}}<form class="labeladd" method="post" action="/ui/messages/{{.Message.ID}}/labels"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="action" value="add"><input name="label" list="mail-label-suggestions" placeholder="Add label…" aria-label="Add label" autocomplete="off" maxlength="64" required><datalist id="mail-label-suggestions">{{range .MessageLabels}}{{if and .CanAdd (not .Selected)}}<option value="{{.Name}}"></option>{{end}}{{end}}</datalist><button class="btn-sm" title="Add label" aria-label="Add label">+</button></form>{{end}}{{if and .Inbox .MoveFolders (.Principal.CanAssist .Inbox.ID)}}<form class="moveto" method="post" action="/ui/messages/{{.Message.ID}}/move"><input type="hidden" name="_csrf" value="{{.CSRF}}"><select name="folder_id" aria-label="Move to">{{range .MoveFolders}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select><button class="btn-sm" title="Move message" aria-label="Move message">Move</button></form>{{end}}</div>
{{if .MessageLabels}}<div class="labelbar"><b>Labels:</b>{{range .MessageLabels}}{{if .Selected}}{{if .CanRemove}}<form class="labelpill" method="post" action="/ui/messages/{{$.Message.ID}}/labels"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="remove"><input type="hidden" name="label_id" value="{{.ID}}"><span>{{.Name}}</span><button class="labelx" title="Remove label" aria-label="Remove label">×</button></form>{{else}}<span class="labelpill">{{.Name}}</span>{{end}}{{end}}{{end}}</div>{{end}}
{{if .Attachments}}<h3>Attachments</h3><ul class="attachments">{{range .Attachments}}<li><a href="/ui/attachments/{{.ID}}">{{.Filename}}</a> <span class="muted">· {{bytes .Size}}</span></li>{{end}}</ul>{{end}}
{{if .RemoteBody}}<hr><div data-remote-body data-body-url="{{.RemoteBodyURL}}" data-html-url="{{.RemoteHTMLURL}}" data-read-url="{{.RemoteReadURL}}" data-attach-url="/ui/messages/{{.Message.ID}}/attachments/{part}" data-csrf="{{.CSRF}}" data-unread="{{if not .Message.Read}}1{{end}}"><div class="load-status pending" role="status" aria-busy="true">Loading message body…</div><noscript>Enable JavaScript to load the message body, or <a href="{{.RemoteHTMLURL}}">open its HTML body</a>.</noscript></div>
{{else}}<hr>{{if .Message.HTML}}{{if .MessageHasRemoteImages}}<div class="banner warn" id="remote-img-banner" style="margin-bottom:8px">Remote images are hidden to prevent read-tracking. <button type="button" class="secondary btn-sm" data-remote-img-show>Show images</button></div>{{end}}<iframe class="mailframe" sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" loading="lazy" src="/ui/messages/{{.Message.ID}}/html" data-mailframe></iframe>{{else}}<div class="msgbody">{{linkify .Message.Text}}</div>{{end}}
{{if and .Message.HTML .Message.Text}}<details><summary>Plain text</summary><div class="msgbody">{{linkify .Message.Text}}</div></details>{{end}}{{end}}</section>
{{if gt (len .ThreadMessages) 1}}<section class="card"><h3>Conversation ({{len .ThreadMessages}})</h3><table>{{range .ThreadMessages}}<tr><td class="muted">{{localDateTime .CreatedAt}}</td><td>{{if eq .Direction "outbound"}}To: {{join .To ", "}}{{else}}{{.From.Address}}{{end}}</td><td>{{if eq .ID $.Message.ID}}<b>{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</b>{{else}}<a href="/ui/messages/{{.ID}}">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</a>{{end}}</td></tr>{{end}}</table></section>{{end}}</div></div>`

func (s *Server) uiMessage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox's messages are cached remote metadata, not local
		// rows; resolve an opaque remote id through the remote reader.
		if s.remoteMessagePage(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	if !m.Read {
		read := true
		if ev, uerr := s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read); uerr == nil {
			m.Read = true
			s.publishStateEvent(ev)
		}
	}
	atts, _ := s.Service.Store.ListAttachments(r.Context(), p, m.ID)
	var box *model.Inbox
	if b, e := s.Service.Store.GetInbox(r.Context(), p, m.InboxID); e == nil {
		box = &b
	}
	var thread []model.Message
	if m.ThreadID != "" {
		thread, _ = s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{InboxID: m.InboxID, ThreadID: m.ThreadID, Limit: 200})
		for i, j := 0, len(thread)-1; i < j; i, j = i+1, j-1 {
			thread[i], thread[j] = thread[j], thread[i]
		}
	}
	title := m.Subject
	if title == "" {
		title = "(no subject)"
	}
	outboundReady := box == nil
	inboundReady := box == nil
	domainSendingURL, domainReceivingURL := "/", "/"
	pausedExternal, pausedAddress, pausedURL := false, "", ""
	if box != nil {
		domainSendingURL = "/?domain=" + url.PathEscape(box.DomainID) + "&kind=sending"
		domainReceivingURL = "/?domain=" + url.PathEscape(box.DomainID) + "&kind=receiving"
		_, outErr := s.Service.Store.ResolveDomainSendingConfig(r.Context(), p.AccountID, box.DomainID)
		outboundReady = outErr == nil
		recvDomain, inErr := s.Service.Store.GetDomain(r.Context(), p.AccountID, box.DomainID)
		inboundReady = inErr == nil && recvDomain.ReceivingProvider != ""
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	// Sidebar context for the message view: counts and the active folder so the
	// shared folder navigation renders consistently with the mailbox views.
	messageFolder := "inbox"
	if m.DeletedAt != nil {
		messageFolder = "trash"
	} else if m.Direction == "outbound" {
		messageFolder = "sent"
	}
	var unreadCount, spamCount, trashCount, draftCount, outboxCount int
	var inboxLabels []string
	var labelUnread map[string]int
	if box != nil {
		unread := s.inboxUnreadCounts(r.Context(), p)
		unreadCount = unread[box.ID]
		spamCount, _ = s.Service.Store.CountSpam(r.Context(), p, box.ID)
		trashCount, _ = s.Service.Store.CountTrash(r.Context(), p, box.ID)
		draftCount, _ = s.Service.Store.CountDrafts(r.Context(), p, box.ID)
		outboxCount, _ = s.Service.Store.CountOutbox(r.Context(), p, box.ID)
		inboxLabels, _ = s.Service.Store.ListInboxLabels(r.Context(), p, box.ID)
		labelUnread = s.unifiedLabelUnreadCounts(r.Context(), p, *box)
	}
	var unifiedLabels []mailLabel
	if box != nil {
		unifiedLabels, _ = s.mailboxLabels(r.Context(), p, *box)
		unifiedLabels = s.selectedMailboxLabels(r.Context(), p, *box, unifiedLabels, m)
	}
	msgData := pageData{Title: title, Page: "inbox", ShowOneReconnect: true, Principal: p, CSRF: csrf(r), Account: acc, Message: &m, MessageHasRemoteImages: htmlsanitize.HasRemoteImages(m.HTML), Attachments: atts, Inbox: box, Folder: messageFolder, Labels: inboxLabels, UnifiedLabels: unifiedLabels, MessageLabels: unifiedLabels, LabelUnread: labelUnread, UnreadCount: unreadCount, SpamCount: spamCount, TrashCount: trashCount, DraftCount: draftCount, OutboxCount: outboxCount, ThreadMessages: thread, Notice: r.URL.Query().Get("notice"), OutboundReady: outboundReady, InboundReady: inboundReady, SendingPausedExternal: pausedExternal, SendingPausedAddress: pausedAddress, SendingPausedURL: pausedURL, DomainSendingSettingsURL: domainSendingURL, DomainReceivingSettingsURL: domainReceivingURL}
	if box != nil {
		s.applyStandaloneMailbox(r, p, *box, &msgData)
		s.applyMailboxSidebar(r.Context(), p, *box, &msgData)
	}
	msgData.UnifiedLabels = sidebarLabels(unifiedLabels)
	s.render(w, r, messageBody, msgData)
}

// aliasNamesCSV returns a comma-joined name list aligned by index with the
// inbox's alias addresses, for the edit dialog's parallel data attributes.
// Alias display names may not contain commas, so the encoding is unambiguous.
func aliasNamesCSV(addresses []string, names map[string]string) string {
	out := make([]string, len(addresses))
	for i, addr := range addresses {
		out[i] = names[addr]
	}
	return strings.Join(out, ",")
}

// domainNameOf returns the lowercased domain part of an email address, or "".
func domainNameOf(address string) string {
	at := strings.LastIndex(address, "@")
	if at < 0 || at == len(address)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(address[at+1:]))
}

// shortTooltipURL keeps connector tooltips compact while preserving both ends
// of a long webhook URL. It slices by rune: a URL can contain non-ASCII
// (IDN hosts, percent-decoded paths), and byte slicing would cut a rune in
// half and render a corrupt character in the tooltip.
func shortTooltipURL(raw string) string {
	raw = strings.TrimSpace(raw)
	const max = 56
	r := []rune(raw)
	if len(r) <= max {
		return raw
	}
	const tail = 14
	return string(r[:len(r)-tail-4]) + "...." + string(r[len(r)-tail:])
}

func snippetText(v string, n int) string {
	v = strings.Join(strings.Fields(v), " ")
	r := []rune(v)
	if len(r) <= n {
		return v
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// formatMailDate renders a stored (UTC) timestamp in loc as a short date.
func formatMailDate(loc *time.Location, t time.Time) string {
	return t.In(loc).Format("15:04 2-Jan-06")
}

// lastUsedDate renders a last-used timestamp for display. A zero time means the
// credential has never authenticated; templates cannot test that with {{if}} on
// a struct, so it is resolved here instead of leaking a year-1 date.
func lastUsedDate(loc *time.Location, t time.Time) string {
	if t.IsZero() {
		return "Never"
	}
	return formatMailDate(loc, t)
}

// passkeyBackup describes one credential's sync reach in words. The WebAuthn
// backup-eligibility flag is fixed at registration, but the backed-up flag can
// flip between ceremonies, so the label is derived from the current pair.
func passkeyBackup(c model.WebAuthnCredential) string {
	switch {
	case !c.BackupEligible:
		return "Bound to this device only"
	case c.BackupState:
		return "Synced to your platform account"
	default:
		return "Can sync between your devices"
	}
}

// formatLocalDateTime renders a stored (UTC) timestamp in loc as a full date.
func formatLocalDateTime(loc *time.Location, t time.Time) string {
	return t.In(loc).Format("2006-01-02 15:04")
}

func filesize(n int64) string {
	const unit = 1024 * 1024
	return fmt.Sprintf("%.1f MB", float64(n)/float64(unit))
}

// inboxSizeClass returns the CSS class for an inbox Size cell: amber at or
// above 90% of the inbox cap, red once the cap is reached, and empty when the
// inbox has no cap of its own (nil pointer) or the quota is unlimited (0).
func inboxSizeClass(quotas map[string]*int64, sizes map[string]int64, id string) string {
	quota := quotas[id]
	if quota == nil || *quota <= 0 {
		return ""
	}
	used := sizes[id]
	switch {
	case used >= *quota:
		return "size-over"
	case used*10 >= *quota*9:
		return "size-near"
	default:
		return ""
	}
}

// inboxSizeTitle builds the Size cell tooltip: the used-of-cap percentage for a
// capped inbox, otherwise the plain usage.
func inboxSizeTitle(quotas map[string]*int64, sizes map[string]int64, id string) string {
	used := sizes[id]
	quota := quotas[id]
	if quota == nil || *quota <= 0 {
		return filesize(used) + " stored"
	}
	pct := int(used * 100 / *quota)
	if pct > 999 {
		pct = 999
	}
	return fmt.Sprintf("%s of %s stored (%d%%)", filesize(used), filesize(*quota), pct)
}
