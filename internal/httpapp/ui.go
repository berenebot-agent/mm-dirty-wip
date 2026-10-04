package httpapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/htmlsanitize"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/timezone"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

const pageTemplate = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · MailMoose</title><link rel="icon" href="/favicon.ico" sizes="any"><link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="apple-touch-icon" href="/apple-touch-icon.png"><style>
body{font:15px system-ui,sans-serif;max-width:none;margin:0;padding:24px 32px;color:#202124;background:#fafafa}a{color:#1557b0}header{display:flex;flex-direction:row;align-items:center;gap:16px;flex-wrap:wrap;margin-bottom:16px}.brandrow{display:flex;align-items:center;gap:8px}.menu{display:flex;align-items:center;gap:16px;flex-wrap:wrap;flex:1;min-width:0}.menu-left,.menu-right{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.menu-right{margin-left:auto}.menu form{margin:0}.quota{font-size:13px;color:#666;white-space:nowrap;padding:0 4px}h1,h2,h3{margin:.4em 0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,300px),1fr));gap:16px}.dashboard-grid{grid-template-columns:repeat(2,minmax(0,1fr))}.dashboard-full-card{grid-column:1/-1}@media (max-width:720px){.dashboard-grid{grid-template-columns:1fr}}.search-form{display:flex;align-items:center;gap:8px;margin:4px 0 12px}.search-form input{flex:1;min-width:0;margin:0}.search-form button{flex:0 0 auto}.provider-picker{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:4px 0 12px}.provider-picker select{margin:0;flex:1;min-width:180px;max-width:320px}.cfg-form .dialog-actions{margin-top:12px}.cfg-summary{display:grid;grid-template-columns:minmax(90px,max-content) 1fr;gap:2px 12px;margin:8px 0;font-size:13px}.cfg-summary dt{color:#666}.cfg-summary dd{margin:0;overflow-wrap:anywhere;min-width:0}.actions-left{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin:8px 0}.actions-left form{margin:0}.wrap{overflow-wrap:anywhere;word-break:break-all}.table-wrap{overflow-x:auto}.card{background:white;border:1px solid #ddd;border-radius:10px;padding:16px;margin-bottom:16px;display:flex;flex-direction:column;min-width:0}.card[hidden]{display:none}.muted{color:#666}input,select,textarea{font:inherit;padding:8px;border:1px solid #bbb;border-radius:6px;box-sizing:border-box}input,select,textarea{width:100%;margin:4px 0 10px}.btn,button{display:inline-block;font:inherit;padding:8px 14px;border:1px solid #111;border-radius:6px;background:#111;color:#fff;text-decoration:none;line-height:1.2;cursor:pointer;box-sizing:border-box}.btn:hover,button:hover{background:#000;border-color:#000}.secondary{background:#fff;color:#111;border-color:#bbb}.btn.secondary:hover,button.secondary:hover{background:#f2f3f5;border-color:#999}.danger{color:#b00020;border-color:#e0a0aa}.btn.danger:hover,button.danger:hover{background:#fdecef;border-color:#c66}.actions{display:flex;gap:8px;align-items:center;justify-content:flex-end;flex-wrap:wrap}.actions form{margin:0}.btn-sm{height:30px;padding:0 10px;font-size:13px;display:inline-flex;align-items:center;justify-content:center}.row .btn-narrow{padding:8px 7px;flex:0 0 auto}.sub{font-size:12px;color:#666;margin-top:2px}.row{display:flex;gap:8px;align-items:center}.row .email-field{margin:0;flex:1}.row>*{flex:1}.slist{list-style:none;margin:0 0 12px;padding:0;border:1px solid #ddd;border-radius:8px;overflow:hidden}.slist li{display:flex;align-items:center;gap:8px;padding:8px 10px;border-bottom:1px solid #eee}.slist li:last-child{border-bottom:0}.slist .addr{flex:1;font-size:14px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist .empty{color:#666;font-size:13px;padding:12px}.slist .icon-btn{flex:0 0 auto}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:8px;border-bottom:1px solid #eee;vertical-align:top}table.dense th,table.dense td{padding:4px 8px;line-height:1.3;vertical-align:middle}table.dense .icon-btn{height:26px;width:26px}table.dense .unread-pill,table.dense .pending-pill{padding:2px 10px}code,pre{background:#f3f3f3;padding:2px 4px;border-radius:4px}pre{padding:12px;white-space:pre-wrap;overflow:auto}.secret{border:1px solid #d5b400;background:#fffbe6;padding:12px;border-radius:8px;word-break:break-all}.secret pre{background:transparent;padding:0;margin:8px 0 0;white-space:pre-wrap;word-break:break-all}.copy-note{font-size:13px;color:#8a6d00;margin-top:8px}.msgbody{white-space:pre-wrap}.pill{display:inline-block;background:#eee;border-radius:999px;padding:2px 7px;font-size:12px}.connector-chips{display:flex;align-items:center;gap:5px;flex-wrap:wrap}.connector-chip{height:28px;width:28px;padding:0;border:0;background:transparent;font-size:12px;gap:0}.connector-chip svg,.connector-chip img{width:18px;height:18px;display:block;object-fit:contain}button.connector-chip:hover,button.connector-chip:focus-visible{background:transparent;border-color:transparent}.connector-add{height:28px;min-width:28px;padding:0 8px}.connector-view-hidden{display:none!important}.connector-list{display:flex;flex-direction:column;gap:8px;margin-top:10px}.connector-row{display:flex;align-items:center;gap:8px;border:1px solid #e3e3e3;border-radius:8px;padding:9px 10px;background:#fff}.connector-type-icon{width:26px;height:26px;display:inline-flex;align-items:center;justify-content:center;flex:0 0 26px;color:#5f6368}.connector-type-icon svg,.connector-type-icon img{width:20px;height:20px;display:block;object-fit:contain}.connector-brand-openclaw{filter:grayscale(1) brightness(0)}.connector-row-main{flex:1;min-width:0;text-align:left}.connector-row-name{font-weight:600}.connector-row-meta{font-size:12px;color:#5f6368;margin-top:2px}.connector-editor{margin-top:0}.connector-editor .dialog-actions{position:static;border-top:0;padding:0;margin-top:12px}.connector-editor .row{align-items:flex-end}.subdomain-tag{display:inline-flex;align-items:center;vertical-align:middle;color:#8a9096}.subdomain-tag svg{width:14px;height:14px;display:block}.log-table{font-size:12px}.log-detail{font-size:10px;word-break:break-all}.error{background:#fee;border:1px solid #e99;padding:10px}.ok{background:#efe;border:1px solid #9c9;padding:10px}dialog{border:0;border-radius:10px;padding:20px;max-width:480px;width:92%;box-sizing:border-box}dialog#inbox-dialog{width:560px;max-width:calc(100vw - 48px)}dialog#inbox-dialog.inbox-dialog--wide{width:880px}dialog#inbox-edit-dialog{width:880px;max-width:calc(100vw - 48px)}#key-dialog{width:820px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}#key-dialog.key-dialog--wide{width:820px;max-width:calc(100vw - 24px)}#key-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0}dialog::backdrop{background:rgba(0,0,0,.45)}.toolbar{display:flex;gap:12px;align-items:center;margin-bottom:12px}.toolbar a{text-decoration:none}.msghead{display:flex;justify-content:space-between;align-items:flex-start;gap:12px;flex-wrap:wrap}.msghead h1{margin-top:0}.inboxhead{display:flex;align-items:center;gap:12px;flex-wrap:wrap}.inboxtitle{margin:0;font-size:1.9em;display:flex;align-items:center;gap:10px;flex-wrap:wrap}.inboxaddr{font-size:14px;color:#5f6368;font-weight:400;cursor:pointer;border-radius:6px;padding:3px 6px;margin:-3px -6px;transition:background .12s,color .12s}.inboxaddr:hover{background:#f2f3f5;color:#202124}.inboxaddr:focus-visible{outline:2px solid #1557b0;outline-offset:1px}.inboxaddr.copied{background:#e6f4ea;color:#137333}.inboxbar{display:flex;gap:10px;align-items:center;margin:10px 0 16px}.inboxbar .active{background:#e8eaed;border-color:#999;font-weight:700}.inboxbar .active:hover{background:#dde1e6;border-color:#777}.inboxbar .icon-btn{height:36px;padding:0 10px}.bulkbar{display:flex;gap:8px;align-items:center;margin-left:auto}.mailheader{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;margin:0 -16px;padding:0 12px 8px;color:#5f6368;font-size:12px;text-transform:uppercase;letter-spacing:.04em;border-bottom:1px solid #e5e5e5}.mailheader>span{text-align:left}.mailheader>span.hcenter{text-align:center}.hcenter{text-align:center}.mailrows{margin:0 -16px -16px}.mailrow{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;border-bottom:1px solid #eee;background:#f2f3f5;padding:0 12px}.mailrow:last-child{border-bottom:0}.mailrow.unread{background:#fff}.mailcheck{display:flex;align-items:center;justify-content:center}.mailcheck input[type=checkbox]{width:15px;height:15px;margin:0}.mailrowlink{grid-column:2 / 7;display:grid;grid-template-columns:22px minmax(150px,220px) 1fr 110px 84px;gap:8px;align-items:center;padding:12px 0;text-decoration:none;color:#5f6368;min-width:0}.mailrow.unread .mailrowlink{color:#202124}.mailrow.unread .mailsender,.mailrow.unread .mailsubject{font-weight:700}.mailsender,.mailsubject,.mailsnippet,.maildate,.mailsize{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.mailsnippet{color:#5f6368;font-weight:400}.maildate,.mailsize{font-size:13px;color:#5f6368;text-align:center}.mailrow.unread .maildate,.mailrow.unread .mailsize{color:#202124}.mailaction{grid-column:7;display:flex;justify-content:center;align-items:center}.mailaction form{margin:0}.mailaction .btn-sm{margin:0 2px}.mailrow.outbox{grid-template-columns:22px minmax(150px,220px) 1fr 110px 150px}.mailrow.outbox .mailrowlink{grid-column:1 / 5;grid-template-columns:22px minmax(150px,220px) 1fr 110px}.mailrow.outbox .mailaction{grid-column:5;justify-content:flex-end;gap:6px}.mailheader.drafts{grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 250px}.mailrow.drafts{grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 250px}.mailrow.drafts .mailrowlink{grid-column:2 / 6;grid-template-columns:22px minmax(150px,220px) 1fr 110px}.mailrow.drafts .mailaction{grid-column:6;justify-content:flex-end;gap:6px}.mailaction button{width:112px;text-align:center}.maildot{display:inline-block}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#1557b0}.unread-pill{background:#1557b0;color:#fff;font-size:13px;font-weight:600;padding:4px 13px;min-width:20px;text-align:center;font-variant-numeric:tabular-nums}.pending-pill{background:#1a73e8;color:#fff;font-size:13px;font-weight:600;padding:4px 13px;min-width:20px;text-align:center;text-decoration:none;font-variant-numeric:tabular-nums}.banner{padding:10px 12px;border-radius:8px;margin-bottom:16px;border:1px solid}.banner.warn{background:#fff8e1;border-color:#e6c34a}.inbox-flags{width:56px;text-align:center;vertical-align:middle!important}.inbox-flags>span{display:inline-flex;align-items:center;justify-content:center;width:20px;height:20px;margin:0 2px;vertical-align:middle}.inbox-flags>span[style]{color:#5f6368}.inbox-flags>span svg{width:14px;height:14px}.draft-status{display:inline-flex;align-items:center;gap:5px;border-radius:999px;padding:4px 9px;font-size:13px;font-weight:600;line-height:1.1;white-space:nowrap}.draft-status svg{width:15px;height:15px;display:block}.draft-status.pending{background:#fff8e1;color:#8a6d00;border:1px solid #e6c34a}.draft-status.rejected{background:#fee;color:#b00020;border:1px solid #e99}.draft-status.sent{background:#eee;color:#333;border:1px solid #ccc}.mailframe{width:100%;height:520px;border:1px solid #ddd;border-radius:8px;background:#fff}.attachments{list-style:none;padding:0;margin:8px 0}.attachments li{padding:4px 0}.brand{color:#202124;text-decoration:none}.brand-logo{display:block;height:44px;width:auto;max-width:100%}.org{font-weight:600;font-size:16px;color:#202124;max-width:40vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.card-head{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:8px}.card-head h2{margin:0}.small{font-size:13px}.icon-btn{height:30px;padding:0 7px;line-height:1;display:inline-flex;align-items:center;justify-content:center}.icon-btn svg{width:16px;height:16px;display:block}.dialog-actions{display:flex;justify-content:flex-end;align-items:center;gap:8px;margin-top:16px}.dialog-actions>form{margin:0;margin-right:auto}.dialog-danger{display:flex;gap:8px;align-items:center;margin-right:auto}.email-field{display:flex;align-items:stretch;margin:4px 0 10px}.email-field input,.email-field select{width:auto;margin:0}.email-field input{flex:1;border-radius:6px 0 0 6px}.email-field .at{display:flex;align-items:center;padding:0 8px;color:#666;background:#f2f3f5;border:1px solid #bbb;border-left:0;border-right:0}.email-field select{border-radius:0 6px 6px 0;border-left:0;max-width:50%}.row-link{cursor:pointer}.row-link:hover td{background:#f6f9ff}.key-fields[data-type=api]>label:first-child{display:flex;align-items:center;gap:8px;margin:4px 0 10px}.key-fields[data-type=api]>label:first-child input{width:auto;margin:0}.key-matrix{width:100%;margin:6px 0}.key-matrix th,.key-matrix td{padding:6px 8px;border-bottom:1px solid #eee;vertical-align:middle}.key-matrix td:last-child,.key-matrix th:last-child{text-align:right}.key-matrix th:last-child{white-space:nowrap}.key-matrix tr.domain-row td{background:#f7f8fa;font-weight:600}.key-matrix tbody[data-domain]+tbody[data-domain] tr:first-child td{border-top:2px solid #e0e0e0}.key-matrix td.domain-inbox{padding-left:20px}.seg{position:relative;display:inline-flex;border:1px solid #bbb;border-radius:7px;overflow:hidden;background:#fff}.seg input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0}.seg label{display:inline-block;min-width:92px;text-align:center;box-sizing:border-box;margin:0;padding:5px 11px;font-size:13px;line-height:1.2;color:#333;cursor:pointer;user-select:none;border-left:1px solid #ddd}.seg label:first-of-type{border-left:0}.seg input:checked+label{background:#111;color:#fff}.seg input:focus-visible+label{outline:2px solid #1557b0;outline-offset:-2px}.seg input:disabled+label{cursor:not-allowed}.seg button{border:0;border-left:1px solid #ddd;border-radius:0;background:#fff;color:#333;min-width:92px;text-align:center;box-sizing:border-box;font-weight:700;padding:5px 11px;font-size:13px;line-height:1.2}.seg button:first-of-type{border-left:0}.seg button:hover{background:#f2f3f5;color:#333}.seg button:disabled{color:#999}.seg button.active{background:#111;color:#fff}.seg button.active:hover{background:#000;color:#fff}.role-legend{width:100%;margin-top:12px;font-size:13px}.role-legend th,.role-legend td{padding:5px 8px;border-bottom:1px solid #eee;text-align:left}.role-legend td:first-child{white-space:nowrap;font-weight:600}.amber{background:#fff8e1;color:#8a6d00;border-color:#e6c34a}button.amber:hover{background:#fdf0c8;border-color:#c9a52f}#key-rotate{margin-right:auto}button:disabled{opacity:.4;cursor:not-allowed}button:disabled:hover{background:#111;border-color:#111}.notice{position:fixed;top:16px;left:50%;transform:translateX(-50%);z-index:100;max-width:min(92vw,560px);box-shadow:0 6px 20px rgba(0,0,0,.15);cursor:pointer;animation:notice-in .2s ease-out}.notice.dismissing{animation:notice-out .3s ease-in forwards}@keyframes notice-in{from{opacity:0;transform:translate(-50%,-8px)}to{opacity:1;transform:translate(-50%,0)}}@keyframes notice-out{to{opacity:0;transform:translate(-50%,-8px)}}@media (prefers-reduced-motion:reduce){.notice{animation:none}.notice.dismissing{animation:none;opacity:0}}.tab{padding:10px 18px;text-decoration:none;color:#111;background:#fff;border:1px solid #bbb;border-radius:6px;font-size:15px;font-weight:600;line-height:1.2;cursor:pointer}.tab:hover{background:#f2f3f5;border-color:#999;color:#111}.tab.active{background:#111;border-color:#111;color:#fff}.tab.active:hover{background:#000;border-color:#000;color:#fff}.steps{margin:8px 0 16px;padding-left:20px}.steps li{margin:6px 0}.cf-code{max-height:420px;overflow:auto}.domains-table{table-layout:fixed}.domains-table th,.domains-table td{padding:8px 4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.domains-table td:first-child b{font-weight:400;white-space:normal;overflow-wrap:anywhere;word-break:break-word}.domains-table th:first-child,.domains-table td:first-child{width:32%;padding-left:8px}.domains-table th:nth-child(2),.domains-table td:nth-child(2){width:14%}.domains-table th:nth-child(3),.domains-table td:nth-child(3),.domains-table th:nth-child(4),.domains-table td:nth-child(4){width:18%}.domains-table th:last-child,.domains-table td:last-child{width:18%}.domains-table .cell-edit{min-width:0;width:100%;max-width:100%;height:30px;min-height:30px;display:inline-flex;align-items:center;justify-content:center;padding:0 4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;line-height:1.2}.domains-table .domain-catchall-add{width:auto;max-width:100%;height:30px;min-height:30px;padding:0 7px}.domains-table .domain-catchall-link{display:block;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.domains-table .domain-provider-edit{height:30px;min-height:30px;font-size:12px;line-height:1.2;padding:0 4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.domains-table .domain-actions{display:flex;width:max-content;max-width:100%;box-sizing:border-box;align-items:center;justify-content:flex-start;gap:4px;flex-wrap:nowrap}.domains-table .domain-actions .icon-btn{flex:0 0 28px;width:28px;height:28px;padding:0}.cell-edit .muted{color:#666}.inherited{color:#9aa0a6;font-size:12px}.cell-link{background:none;border:0;padding:0;margin:0;font-size:13px;color:#1557b0;cursor:pointer;text-align:left}.cell-link:hover{background:none;border:0;text-decoration:underline}.provider-select{max-width:320px}.provider-hint{margin:0}.inherit-option{display:flex;align-items:flex-start;gap:8px;margin:6px 0;width:auto;min-width:0;text-align:left}.inherit-option input[type=checkbox]{width:15px;height:15px;margin:2px 0 0;flex:0 0 auto}.inherit-option span{min-width:0}.provider-box{border:1px solid #ddd;border-radius:8px;padding:12px 12px 4px;margin-top:8px;background:#fafafa}.domain-dialog{width:640px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}.cf-setup-dialog{width:720px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}.cf-setup-dialog h2:first-child,.cf-setup-dialog>div>h2:first-child{margin-top:0}.cf-setup-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0;margin-top:8px}.domain-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0;margin-top:8px}.attach-drop{border:2px dashed #bbb;border-radius:8px;padding:16px;margin:4px 0 10px;background:#fafafa;text-align:center;transition:border-color .15s,background .15s}.attach-drop.dragover{border-color:#1557b0;background:#eef4fd}.attach-hint{margin:0 0 8px;color:#5f6368;font-size:13px}.attach-browse{cursor:pointer}.attach-input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap}.attach-list{list-style:none;margin:10px 0 0;padding:0;text-align:left}.attach-list li{display:flex;align-items:center;gap:10px;padding:6px 8px;border:1px solid #e3e3e3;border-radius:6px;background:#fff;margin-bottom:6px}.attach-list .attach-name{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.attach-list .attach-size{color:#5f6368;font-size:12px;white-space:nowrap}.attach-list .attach-remove{border:0;background:none;color:#5f6368;font-size:18px;line-height:1;padding:0 4px;cursor:pointer}.attach-list .attach-remove:hover{background:none;color:#b00020}.attach-overlay{display:none;position:fixed;inset:0;z-index:200;align-items:center;justify-content:center;background:rgba(21,87,176,.10);border:3px dashed #1557b0;box-sizing:border-box}.attach-overlay.active{display:flex}.attach-overlay-inner{background:#fff;border:1px solid #1557b0;border-radius:10px;padding:16px 28px;font-size:18px;font-weight:600;color:#1557b0;box-shadow:0 8px 24px rgba(0,0,0,.12)}.labelpill{display:inline-flex;align-items:center;gap:4px;background:#e6f4ea;color:#137333;border:1px solid #b7e1c4;border-radius:999px;padding:1px 8px;font-size:11px;vertical-align:middle}.labelpill span{overflow:hidden;text-overflow:ellipsis}.labelx{border:0;background:none;color:#137333;font-size:13px;line-height:1;padding:0 2px;cursor:pointer;box-shadow:none}.labelx:hover{background:none;color:#b00020}.msgmeta{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;flex-wrap:wrap}.msgmeta p{margin:0}.labelbar{display:flex;gap:6px;flex-wrap:wrap;align-items:center;margin:0 0 10px}.labelbar form{margin:0}.labeladd{display:flex;gap:4px;align-items:center;flex:0 0 auto}.labeladd input{width:auto;margin:0;padding:3px 8px;line-height:1.3}.labelbar .btn-sm{padding:2px 8px;font-size:12px}.dialog-tabs{display:flex;gap:6px;flex-wrap:wrap;margin:0 0 14px;padding-bottom:10px;border-bottom:1px solid #eee}.dialog-tab{background:#fff;color:#202124;border:1px solid #bbb;border-radius:999px;padding:5px 13px;font-size:13px;line-height:1.3}.dialog-tab:hover{background:#f2f3f5}.dialog-tab.active{background:#111;color:#fff;border-color:#111}.dialog-panel{margin:0}.dialog-usage{display:grid;grid-template-columns:minmax(90px,max-content) 1fr;gap:4px 12px;margin:8px 0;font-size:14px}.dialog-usage dt{color:#666}.dialog-usage dd{margin:0}.section-head{margin:14px 0 6px;font-size:14px;font-weight:700}.dialog-panel>.section-head:first-child{margin-top:0}.slist.aliases .alias-row{display:flex;align-items:center;gap:8px}.slist .alias-text{flex:1;min-width:0;display:flex;flex-direction:column;gap:1px}.slist .alias-row-name{font-size:14px;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist .alias-row-name.muted{font-weight:400}.slist .alias-row-addr{font-size:12px;color:#5f6368;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}#alias-dialog{width:420px;max-width:calc(100vw - 24px)}.mail-layout{display:grid;grid-template-columns:210px minmax(0,1fr);gap:20px;align-items:start}.mailnav{position:sticky;top:16px;display:flex;flex-direction:column;gap:4px}.mailnav .compose{width:100%;justify-content:center;margin-bottom:8px}.mailnav a.folder{display:flex;align-items:center;justify-content:space-between;gap:8px;padding:8px 12px;border-radius:8px;color:#202124;text-decoration:none;font-size:15px}.mailnav a.folder:hover{background:#f2f3f5}.mailnav a.folder.active{background:#e8eaed;font-weight:700}.mailnav .count{color:#5f6368;font-size:13px;font-weight:400;font-variant-numeric:tabular-nums}.mailnav .count.unread{background:#1557b0;color:#fff;border-radius:999px;padding:1px 8px}.mailnav .navgroup{margin:10px 0 2px;padding:0 12px;font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.04em;color:#5f6368}.mailnav a.folder.label{padding-left:24px;font-size:14px}.mailnav a.folder.label .labelname{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}@media (max-width:760px){.mail-layout{grid-template-columns:1fr;gap:12px}.mailnav{position:static;flex-direction:row;flex-wrap:wrap;align-items:center;border-bottom:1px solid #e5e5e5;padding-bottom:8px}.mailnav .compose{width:auto;margin:0 4px 0 0}.mailnav a.folder{padding:6px 10px}}.mailaction button{width:112px;text-align:center}.mailaction button.icon-btn{width:30px;padding:0}.mailaction .icon-btn{margin:0 2px}.mailaction .icon-btn svg{width:18px;height:18px}.bulkbar .icon-btn{height:30px;width:30px;padding:0}@media (max-width:640px){body{padding:16px}.mailheader{display:none}.mailrow,.mailrow.outbox,.mailrow.drafts{grid-template-columns:26px 1fr;column-gap:8px;row-gap:0;padding:10px 12px}.mailrow.outbox{grid-template-columns:1fr}.mailrow .mailcheck{grid-column:1}.mailrow .maildot{display:none}.mailrow .mailrowlink,.mailrow.outbox .mailrowlink,.mailrow.drafts .mailrowlink{grid-column:2;display:grid;grid-template-columns:1fr auto;column-gap:8px;align-items:center;padding:0;min-width:0}.mailrow.outbox .mailrowlink{grid-column:1}.mailrow .mailsender{display:none}.mailrow .mailsnippet{display:none}.mailrow .mailsize{display:none}.mailrow .mailsubject{grid-column:1;font-size:14px}.mailrow .maildate{grid-column:2;text-align:right;font-size:12px;white-space:nowrap}.mailrow .mailaction{display:none}.mailrow.outbox .mailaction{display:flex;grid-column:1;justify-content:flex-end;margin-top:6px}.mailrow.outbox .maildate{grid-column:2;grid-row:1}.mailrow.outbox .mailsubject{grid-column:1}.mailrow.outbox .mailsender{grid-column:1;grid-row:2;display:block;font-size:12px}.mailrow.drafts .mailaction{display:flex;grid-column:1 / -1;justify-content:flex-end;margin-top:6px}.mailrow.drafts .mailsubject{grid-column:1}.mailrow.drafts .maildate{grid-column:2;grid-row:1}.mailrow.drafts .mailsender{grid-column:1;grid-row:2;display:block;font-size:12px}.inboxhead,.inboxbar,.toolbar,.bulkbar{flex-wrap:wrap}.inboxbar .bulkbar{margin-left:0}.card{padding:12px}.grid,.dashboard-grid{grid-template-columns:1fr}.search-form{flex-wrap:wrap}.provider-picker select{max-width:none}.domains-table{table-layout:auto}.quota{display:none}.menu,.menu-right{gap:8px}h1{font-size:1.5em}.inboxtitle{font-size:1.5em}}</style></head><body><header><div class="brandrow"><a class="brand" href="/" title="Home" aria-label="MailMoose home"><img class="brand-logo" src="{{asset "logo-horizontal.png"}}" alt="MailMoose"></a>{{if .Account.ID}}<span class="org">{{.Account.Name}}</span>{{end}}</div>{{if .Principal.UserID}}<nav class="menu"><div class="menu-right">{{if .Account.ID}}<span class="quota">{{filesize .Account.StorageUsedBytes}} of {{filesize .Account.StorageQuotaBytes}} stored.</span>{{end}}{{if .Principal.SystemAdmin}}<a class="tab{{if eq .Tab "admin"}} active{{end}}" href="/admin">Admin</a>{{end}}<a class="tab{{if eq .Tab "account"}} active{{end}}" href="/account">Account</a><form method="post" action="/logout"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="tab">Log Out</button></form></div></nav>{{end}}</header>{{template "body" .}}<script src="{{asset "app.js"}}" defer></script></body></html>` + inboxTableTemplate

// dialMXSetupView is the per-domain Dial MX setup panel: the public half of the
// domain's exact signing key (never the private seed), the DNS TXT record that
// publishes it, and the live per-receiver status. It is rendered for any domain
// whose effective receiving provider is Dial MX, including a subdomain that
// inherits Dial MX from an ancestor — such a domain still owns its own exact
// key even though its receiver URL list is inherited.
type dialMXSetupView struct {
	KeyID        string
	PublicKey    string
	TXT          string
	ReceiverURLs string
	Statuses     []mxdial.Status
}

// decodePublicKey decodes the canonical base64url public key stored for a Dial
// MX credential. The stored form is unchanged; it is only decoded for the TXT
// record.
func decodePublicKey(raw string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(raw)
	return b
}

// dialMXStatuses snapshots the manager's live per-receiver status for a domain.
// A nil manager (Dial MX disabled) yields no statuses rather than an error.
func (s *Server) dialMXStatuses(domain string) []mxdial.Status {
	if s.Service.DialMX == nil {
		return nil
	}
	return s.Service.DialMX.Status(domain)
}

// dialMXReady reports whether a receiver has accepted this domain's exact key.
// Only a ready receiver advertises a usable MX target.
func dialMXReady(st mxdial.Status) bool { return st.State == "ready" }

// dialMXReason bounds a receiver-supplied rejection reason so an unexpectedly
// long or hostile value cannot distort the setup panel.
func dialMXReason(st mxdial.Status) string {
	reason := strings.TrimSpace(st.Reason)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	return reason
}

// dialMXExpiry renders the remaining authorization window compactly, or "" when
// the receiver did not issue a bounded expiry.
func dialMXExpiry(st mxdial.Status) string {
	if st.ExpiresAt.IsZero() {
		return ""
	}
	d := time.Until(st.ExpiresAt)
	if d <= 0 {
		return ""
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// render renders body with timestamp helpers bound to the request's effective
// display zone, so the human UI shows local time without changing stored or API
// timestamps.
func (s *Server) render(w http.ResponseWriter, r *http.Request, body string, data any) {
	t, err := template.New("page").Funcs(s.templateFuncs(requestTZ(r))).Parse(pageTemplate + `{{define "body"}}` + body + `{{end}}`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err = t.Execute(w, data); err != nil {
		s.Log.Error("render", "error", err)
	}
}

// templateFuncs builds the template helper set. Formatting helpers close over
// loc so they can render UTC timestamps in the viewer's zone.
func (s *Server) templateFuncs(loc *time.Location) template.FuncMap {
	if loc == nil {
		loc = time.UTC
	}
	return template.FuncMap{
		"bytes":           formatBytes,
		"join":            strings.Join,
		"snippet":         snippetText,
		"mailDate":        func(t time.Time) string { return formatMailDate(loc, t) },
		"localDateTime":   func(t time.Time) string { return formatLocalDateTime(loc, t) },
		"filesize":        filesize,
		"shortURL":        shortTooltipURL,
		"asset":           s.assetURL,
		"linkify":         linkifyText,
		"querystring":     url.QueryEscape,
		"aliasNames":      aliasNamesCSV,
		"externalAliases": externalAliasesJSON,
		"connectorsJSON":  connectorsJSON,
		"deliveryStatus":  deliveryStatus,
		"dialmxReady":     dialMXReady,
		"dialmxReason":    dialMXReason,
		"dialmxExpiry":    dialMXExpiry,
	}
}

type pageData struct {
	Title                     string
	Tab                       string
	Principal                 model.Principal
	CSRF                      string
	Account                   model.Account
	User                      model.User
	Domains                   []model.Domain
	DomainSendingReady        map[string]bool
	DomainReceivingReady      map[string]bool
	DomainIsMX                map[string]bool
	InboxSendingReady         map[string]bool
	Domain                    *model.Domain
	DomainInboxes             map[string][]model.Inbox
	DomainSendingEditors      map[string][]*domainEditorView
	DomainReceivingEditors    map[string][]*domainEditorView
	DomainSendingSelected     map[string]string
	DomainReceivingSelected   map[string]string
	DomainSendingLabel        map[string]string
	DomainReceivingLabel      map[string]string
	DomainReceivingRegenerate map[string]bool
	// DialMXSetup is keyed by domain id. The value is a pointer so a lookup for
	// an unconfigured domain yields nil, which templates treat as absent.
	DialMXSetup map[string]*dialMXSetupView
	// DomainParentCandidate maps a domain id to the name of the nearest existing
	// ancestor domain, for a domain that was added before its parent and is not
	// linked yet. It lets the sending/receiving provider menus offer "Inherited
	// (from <parent>)", which links the domain on save.
	DomainParentCandidate map[string]string
	DomainOpenID          string
	DomainOpenKind        string
	DomainWorkerCode      string
	DomainWorkerWebhook   string
	// DomainNamesCSV lists the account's domain names (comma separated) so the
	// Add Domain dialog can detect a typed subdomain client-side and offer to
	// reuse the parent's connectors. Server-side detection is authoritative.
	DomainNamesCSV             string
	DomainSendingSettingsURL   string
	DomainReceivingSettingsURL string
	// Sending-paused banner: when the selected/default sender is an external
	// alias whose connector is missing, point the operator at that alias rather
	// than the domain sending editor.
	SendingPausedExternal bool
	SendingPausedAddress  string
	SendingPausedURL      string
	// ExternalAliasDialogs drives the domain-style connector popups rendered on
	// the dashboard and the alias activity page. It is empty where not needed.
	ExternalAliasDialogs []externalAliasDialogView
	// InboxOpenID, when set, is the inbox whose edit dialog the dashboard should
	// reopen on load. InboxOpenTab selects the tab and InboxOpenConnectorID can
	// focus one connector after a connector settings action.
	InboxOpenID          string
	InboxOpenTab         string
	InboxOpenConnectorID string
	Inboxes              []model.Inbox
	Messages             []model.Message
	Credentials          []credentialView
	InboxConnectors      map[string][]credentialView
	// Passkeys lists the signed-in user's registered passkeys, and
	// PasskeyEnabled reports whether the deployment has WebAuthn configured.
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
	HasUsers                    bool
	BaseURL                     string

	Inbox          *model.Inbox
	InboxAddr      map[string]string
	Unread         map[string]int
	MailboxSizes   map[string]int64
	UnreadCount    int
	SpamCount      int
	TrashCount     int
	DraftCount     int
	OutboxCount    int
	HasMore        bool
	Before         string
	Folder         string
	PagerURL       string
	Labels         []string
	LabelUnread    map[string]int
	ActiveLabel    string
	ThreadMessages []model.Message
	OutboundReady  bool
	InboundReady   bool

	ComposeTitle, ComposeAction, ComposeCancel string
	ComposeTo, ComposeCC, ComposeBCC           string
	ComposeSubject, ComposeText, ComposeNote   string
	ComposeError, ComposeFlash                 string
	ComposeDraftID                             string
	ComposeFrom                                string
	ComposeFromOptions                         []fromOption

	Drafts []model.Draft

	// External sending alias activity page.
	ExternalAlias            *store.ExternalAlias
	ExternalDeliveryAttempts []store.DeliveryAttempt
	ExternalLogHasMore       bool
	ExternalLogBefore        int64

	// Admin plane and account management: the account mailer selection, the
	// invite list, the one-time setup link shown immediately after creation,
	// and the account's mailbox operators.
	AccountMailerInboxID string
	Invites              []inviteView
	InviteLink           string
	Operators            []memberView
	Accounts             []store.AccountSummary
	// Installation MX receiver panel shown on /admin. MXForm carries the
	// non-secret editable values (including any preserved failed submission);
	// the private STARTTLS key is never part of it.
	MXForm              mxFormView
	MXStatus            app.MXReceiverStatus
	MXIncludedSupported bool

	DraftCounts  map[string]int
	SendRequests []SendRequestRow
	ReviewDraft  *model.Draft

	TrashRetentionDays int

	// Timezone settings on the account page: the account default and the
	// signed-in user's override, plus the selectable zone names.
	AccountTimezone string
	UserTimezone    string
	TimezoneOptions []string

	Email string
}

// externalAliasDataView is the secret-free shape embedded in the inbox edit
// button for the Aliases tab's external list: identity plus connector status.
type externalAliasDataView struct {
	ID          string `json:"id"`
	Address     string `json:"address"`
	DisplayName string `json:"display_name,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Configured  bool   `json:"configured"`
}

// externalAliasesJSON marshals an inbox's external aliases to the secret-free
// JSON embedded in the edit button's data attribute. html/template escapes the
// result in attribute context, so the browser decodes it back to valid JSON.
func externalAliasesJSON(aliases []model.ExternalAlias) string {
	views := make([]externalAliasDataView, 0, len(aliases))
	for _, a := range aliases {
		views = append(views, externalAliasDataView{ID: a.ID, Address: a.Address, DisplayName: a.DisplayName, Provider: a.Provider, Configured: a.Configured})
	}
	b, err := json.Marshal(views)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// fromOption is one choice in the compose/reply From select: the submitted
// address and a display label that includes the sender name when set.
type fromOption struct {
	Address string
	Label   string
}

type credentialView struct {
	ID, Kind, Name, Type, Scope, RolesJSON, InboxID, Role string
	URL, Mode, AuthMode                                   string
	Admin                                                 bool
	Enabled                                               bool
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
// domain is ready only when that domain has a sending config, and an external
// sending alias is ready only when its own connector is configured.
func inboxReadiness(domains []model.Domain, boxes []model.Inbox) (sendingReady, receivingReady, inboxSendingReady map[string]bool) {
	sendingReady = make(map[string]bool, len(domains))
	receivingReady = make(map[string]bool, len(domains))
	domainInboxesByName := make(map[string]string, len(domains))
	for _, d := range domains {
		domainInboxesByName[strings.ToLower(d.Name)] = d.ID
		sendingReady[d.ID] = d.SendingProvider != ""
		receivingReady[d.ID] = d.ReceivingProvider != ""
	}
	inboxSendingReady = make(map[string]bool, len(boxes))
	for _, b := range boxes {
		if a, ok := externalSenderFor(b); ok {
			inboxSendingReady[b.ID] = a.Configured
			continue
		}
		effective := b.DomainID
		if name := domainNameOf(b.DefaultSender); name != "" {
			if id, ok := domainInboxesByName[name]; ok {
				effective = id
			}
		}
		inboxSendingReady[b.ID] = sendingReady[effective]
	}
	return sendingReady, receivingReady, inboxSendingReady
}

func dashboardActivityRows(entries []store.DomainLogEntry) []model.Message {
	msgs := make([]model.Message, 0, len(entries))
	for _, entry := range entries {
		direction := "inbound"
		switch entry.Kind {
		case "sent", "failed", "sending", "interrupted":
			direction = "outbound"
		}
		msgs = append(msgs, model.Message{
			ID:        entry.MessageID,
			InboxID:   entry.InboxID,
			Direction: direction,
			From:      model.Address{Address: entry.FromAddress},
			To:        entry.To,
			Subject:   entry.Subject,
			Client:    entry.Client,
			SizeBytes: entry.SizeBytes,
			CreatedAt: entry.At,
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

const authBody = `<div class="card" style="max-width:460px;margin:60px auto"><h1>{{.Title}}</h1>{{if .Notice}}<div class="error">{{.Notice}}</div>{{end}}<form method="post"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Email</label><input type="email" name="email" required value="{{.Email}}"><label>Password</label><input type="password" name="password" minlength="10" required><button>{{.Title}}</button></form>{{if .PasskeyEnabled}}<div style="margin-top:12px"><button type="button" class="secondary" id="passkey-signin" data-begin="/login/webauthn/begin" data-finish="/login/webauthn/finish" style="width:100%">Sign in with a passkey</button><p class="muted small" id="passkey-status" role="status" aria-live="polite"></p></div>{{end}}<div style="margin-top:16px;padding-top:12px;border-top:1px solid #eee"><p style="font-size:14px;margin:0 0 6px">Agents: see <a href="/agent">/agent</a> for API access instructions</p><p class="muted" style="font-size:12px;margin:0">Reference: <a href="/openapi.json">/openapi.json</a> · <a href="/examples/python">/examples/python</a> · <a href="/examples/bash">/examples/bash</a> · <a href="/.well-known/mailmoose">/.well-known/mailmoose</a></p></div></div>`

// unconfiguredBody is shown when the database has no system administrator and
// no ADMIN_EMAIL / ADMIN_PASSWORD credentials were supplied. It is deliberately
// static: there is no unauthenticated form that can claim the instance. The
// operator must set ADMIN_EMAIL and ADMIN_PASSWORD and restart.
const unconfiguredBody = `<div class="card" style="max-width:560px;margin:60px auto"><h1>MailMoose has not been configured</h1><p>Set <code>ADMIN_EMAIL</code> and <code>ADMIN_PASSWORD</code> and restart MailMoose.</p><p class="muted">The system administrator is created from these values; changing them later rotates the login on restart.</p></div>`

type authFlash struct {
	Title, Error, Email string
}

// renderAuth shows an auth page, restoring any error and email left by a
// redirect from a failed POST (Post/Redirect/Get).
func (s *Server) renderAuth(w http.ResponseWriter, r *http.Request, title string) {
	data := pageData{Title: title, CSRF: s.setPreAuthCSRF(w, r), PasskeyEnabled: s.webauthn != nil}
	if v, ok := s.flashes.take(r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(authFlash); ok {
			data.Title, data.Notice, data.Email = f.Title, f.Error, f.Email
		}
	}
	s.render(w, r, authBody, data)
}

// flashAuth stores an auth error and redirects back to the form.
func (s *Server) flashAuth(w http.ResponseWriter, r *http.Request, dest, title, msg, email string) {
	if tok := s.flashes.put(authFlash{Title: title, Error: msg, Email: email}, len(title)+len(msg)+len(email)+32); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// setupGet shows the unconfigured page on a fresh install. There is no POST
// counterpart: an unconfigured instance cannot be claimed over HTTP. The
// operator creates the system administrator by setting ADMIN_EMAIL and
// ADMIN_PASSWORD before startup.
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
	s.render(w, r, unconfiguredBody, pageData{Title: "MailMoose has not been configured"})
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
	if s.registerLimiter != nil && !s.registerLimiter.Allow(ip) {
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
		s.flashAuth(w, r, "/register", "Create Account", err.Error(), r.Form.Get("email"))
		return
	}
	tok, _, _ := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/", 303)
}
func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	s.renderAuth(w, r, "Log In")
}
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.Service.Config)
	if !s.loginLimiter.Allow(ip) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	_ = r.ParseForm()
	u, err := s.Service.Store.AuthenticateUser(r.Context(), r.Form.Get("email"), r.Form.Get("password"))
	if err != nil {
		s.flashAuth(w, r, "/login", "Log In", "Invalid email or password", r.Form.Get("email"))
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

const settingsBody = `<h1>Account</h1>{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
<div class="grid">
<section class="card"><h2>Change account name</h2><p class="muted">Shown in the header. This is a display name, not your email address.</p><form method="post" action="/ui/account/account"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Account name</label><input name="name" value="{{.Account.Name}}" maxlength="80" required><div class="dialog-actions"><button>Save</button></div></form></section>
{{if .User.SystemAdmin}}<section class="card"><h2>Login credentials</h2><p class="muted">Your password is managed by the deployment configuration. Update <code>ADMIN_EMAIL</code> and <code>ADMIN_PASSWORD</code> (or their <code>_FILE</code> secrets) and restart MailMoose; the new password takes effect and other sessions are signed out. This password always works as a recovery method. You may also add passkeys below as an additional, independent way to sign in.</p></section>{{else}}<section class="card"><h2>Change email address</h2><p class="muted">Used to log in. Current: {{.User.Email}}</p><form method="post" action="/ui/account/email"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>New email</label><input type="email" name="email" required><label>Current password</label><input type="password" name="current_password" autocomplete="current-password" required><div class="dialog-actions"><button>Update email</button></div></form></section>
<section class="card"><h2>Change password</h2><form method="post" action="/ui/account/password"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Current password</label><input type="password" name="current_password" autocomplete="current-password" required><label>New password</label><input type="password" name="new_password" minlength="10" autocomplete="new-password" required><label>Confirm new password</label><input type="password" name="confirm_password" minlength="10" autocomplete="new-password" required><div class="dialog-actions"><button>Change password</button></div></form></section>{{end}}
{{if or .Principal.Admin .Principal.OwnsAccount}}<section class="card"><h2>Trash</h2><p class="muted">Deleted messages are moved to Trash and kept until purged. Trashed messages count toward storage until permanently deleted.</p><form method="post" action="/ui/account/trash-retention"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Auto-purge trashed messages after (days)</label><input type="number" name="days" value="{{.TrashRetentionDays}}" min="0" max="3650" required><p class="muted small">Set to 0 to keep trashed messages until you empty the trash manually.</p><div class="dialog-actions"><button>Save</button></div></form></section>{{end}}
<section class="card"><h2>Your time zone</h2><p class="muted">Times are stored and served in UTC. This only changes how they are shown to you in this web interface.</p><form method="post" action="/ui/account/timezone/me"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Time zone</label><input name="timezone" list="tz-options" value="{{.UserTimezone}}" placeholder="Leave blank to use the account default" autocomplete="off"><p class="muted small">Leave blank to follow the account default.</p><div class="dialog-actions"><button>Save</button></div></form></section>
{{if or .Principal.Admin .Principal.OwnsAccount}}<section class="card"><h2>Account time zone</h2><p class="muted">The default display time zone for operators who have not set their own. Times remain stored and served in UTC.</p><form method="post" action="/ui/account/timezone"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Default time zone</label><input name="timezone" list="tz-options" value="{{.AccountTimezone}}" placeholder="UTC" autocomplete="off"><p class="muted small">Leave blank for UTC.</p><div class="dialog-actions"><button>Save</button></div></form></section>{{end}}
{{if .PasskeyEnabled}}<section class="card"><h2>Passkeys</h2><p class="muted">Sign in without a password using a passkey (Touch ID, Windows Hello, or a security key). Add one per device.{{if not .User.SystemAdmin}} When you add a passkey you can choose to make it your only sign-in method.{{end}}{{if not .User.PasswordEnabled}} <b>Password sign-in is currently disabled.</b>{{end}}</p>{{if .Passkeys}}<div class="table-wrap"><table class="dense"><thead><tr><th>Name</th><th>Added</th><th>Last used</th><th>Synced</th><th></th></tr></thead><tbody>{{range .Passkeys}}<tr><td>{{.Name}}</td><td class="muted">{{mailDate .CreatedAt}}</td><td class="muted">{{if .LastUsedAt}}{{mailDate .LastUsedAt}}{{else}}Never{{end}}</td><td class="muted">{{if .BackupEligible}}{{if .BackupState}}Yes{{else}}Syncable{{end}}{{else}}Device only{{end}}</td><td class="actions"><form method="post" action="/ui/account/passkeys/rename"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><input name="name" value="{{.Name}}" maxlength="80" required><button class="secondary btn-sm">Rename</button></form><form method="post" action="/ui/account/passkeys/delete" data-confirm="Remove this passkey?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><button class="secondary btn-sm danger">Remove</button></form></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No passkeys yet.</p>{{end}}<div class="dialog-actions">{{if and (not .User.PasswordEnabled) (not .User.SystemAdmin)}}<form method="post" action="/ui/account/passkeys/password" data-confirm="Re-enable password sign-in?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary">Re-enable password sign-in</button></form>{{end}}<button type="button" class="secondary" id="passkey-add" data-begin="/ui/account/passkeys/begin" data-finish="/ui/account/passkeys/finish" data-password-enabled="{{if and .User.PasswordEnabled (not .User.SystemAdmin)}}1{{else}}0{{end}}">Add a passkey</button></div><p class="muted small" id="passkey-status" role="status" aria-live="polite"></p></section>{{end}}
<datalist id="tz-options">{{range .TimezoneOptions}}<option value="{{.}}"></option>{{end}}</datalist>
</div>` + accountOperatorsSection

// settingsRedirect stores a settings flash and redirects back to the settings
// page (Post/Redirect/Get).
func (s *Server) settingsRedirect(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	dest := "/account"
	if tok := s.flashes.put(settingsFlash{Error: errMsg, Notice: notice}, len(notice)+len(errMsg)+32); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
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
	data := pageData{Title: "Account", Tab: "account", Principal: p, CSRF: csrf(r), Account: acc, User: user,
		AccountTimezone: acc.Timezone, UserTimezone: user.Timezone, TimezoneOptions: timezone.Options()}
	data.PasskeyEnabled = s.webauthn != nil
	if creds, cerr := s.Service.Store.WebAuthnCredentialsForUser(r.Context(), p.UserID); cerr == nil {
		data.Passkeys = creds
	}
	if p.OwnsAccount() {
		if days, derr := s.Service.Store.GetTrashRetention(r.Context(), p); derr == nil {
			data.TrashRetentionDays = days
		}
	}
	// The account page can receive two flash kinds: a settings result and a
	// one-time invitation link. Dispatch on the stored type.
	if tok := r.URL.Query().Get("_flash"); tok != "" {
		if v, ok := s.flashes.peek(tok); ok {
			switch v.(type) {
			case settingsFlash:
				if taken, ok := s.flashes.take(tok); ok {
					if tf, ok := taken.(settingsFlash); ok {
						data.Error, data.Notice = tf.Error, tf.Notice
					}
				}
			case inviteFlash:
				if taken, ok := s.flashes.take(tok); ok {
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

func (s *Server) uiSettingsAccount(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.Store.UpdateAccountName(r.Context(), p.AccountID, r.Form.Get("name")); err != nil {
		s.settingsRedirect(w, r, "", err.Error())
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.rename", "")
	s.settingsRedirect(w, r, "Account name updated", "")
}

// uiSettingsTrashRetention updates the account's Trash auto-purge window. It
// requires an account Owner (or Admin).
func (s *Server) uiSettingsTrashRetention(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.OwnsAccount() {
		s.settingsRedirect(w, r, "", "You do not have permission to change this setting")
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("days")))
	if err != nil || days < 0 {
		s.settingsRedirect(w, r, "", "Retention must be a whole number of days (0 or more)")
		return
	}
	if err := s.Service.Store.SetTrashRetention(r.Context(), p, days); err != nil {
		s.settingsRedirect(w, r, "", err.Error())
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.trash_retention", strconv.Itoa(days))
	s.settingsRedirect(w, r, "Trash retention updated", "")
}

// uiSettingsAccountTimezone sets the account default display time zone. It
// requires an account Owner (or Admin). An empty value means UTC.
func (s *Server) uiSettingsAccountTimezone(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.OwnsAccount() {
		s.settingsRedirect(w, r, "", "You do not have permission to change this setting")
		return
	}
	tz := strings.TrimSpace(r.Form.Get("timezone"))
	if err := s.Service.Store.SetAccountTimezone(r.Context(), p, tz); err != nil {
		s.settingsRedirect(w, r, "", err.Error())
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
		s.settingsRedirect(w, r, "", err.Error())
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
	if !s.passwordLimiter.Allow(ip) {
		s.settingsRedirect(w, r, "", "too many attempts, try again later")
		return
	}
	err := s.Service.Store.UpdateUserEmail(r.Context(), p.UserID, p.AccountID, r.Form.Get("email"), r.Form.Get("current_password"))
	if err != nil {
		msg := err.Error()
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
	if !s.passwordLimiter.Allow(ip) {
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
		msg := err.Error()
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

const dashboardBody = `{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Secret}}<div class="secret"><b>{{.SecretLabel}}</b><pre>{{.Secret}}</pre></div>{{end}}
 {{if .DomainWorkerCode}}<dialog id="cf-setup-dialog" class="cf-setup-dialog" data-open="1"><div id="cf-worker-step"><h2>Cloudflare setup code</h2><p class="muted">Paste this into Cloudflare. It contains the generated shared secret and is shown only once.</p><ol class="steps"><li>In Cloudflare, open <b>Workers &amp; Pages</b> → <b>Create application</b> → <b>Start with Hello World</b> → <b>Deploy</b>.</li><li>Open the Worker, choose <b>Edit code</b>, replace the stub with the code below, then <b>Deploy</b>.</li></ol><pre class="cf-code" id="cf-code">{{.DomainWorkerCode}}</pre><p class="copy-note" id="cf-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the code above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="cf-copy">Copy code</button><button type="button" class="btn" id="cf-next">Next</button></div></div><div id="cf-routing-step" hidden><h2>Email Routing</h2><p class="muted">Now point this domain's mail at the Worker.</p><ol class="steps"><li>In Cloudflare, open <b>Email Routing</b> for this domain and onboard it, adding the <b>DNS records</b> Cloudflare lists.</li><li>In <b>Email Routing</b>, edit the <b>catch-all</b> rule, choose <b>Send to a Worker</b>, and select this Worker.</li><li><b>Enable</b> the catch-all rule.</li></ol><div class="dialog-actions"><button type="button" class="btn" id="cf-done">Done</button></div></div></dialog>{{end}}
<div class="tab-panel"{{if ne .Tab "home"}} hidden{{end}}>
<div class="grid dashboard-grid"><section class="card" style="grid-column:1/-1" data-open-inbox="{{.InboxOpenID}}" data-open-inbox-tab="{{.InboxOpenTab}}" data-open-connector="{{.InboxOpenConnectorID}}"><div class="card-head"><h2>Inboxes</h2><button type="button" id="add-inbox">Add Inbox</button></div>{{if .Inboxes}}{{template "inboxes-table" .}}{{else}}<p class="muted">No inboxes yet.</p>{{end}}</section>
<section class="card"><div class="card-head"><h2>Clients</h2><button type="button" id="add-key">Add Client</button></div>{{if .Credentials}}<div class="table-wrap"><table class="dense"><thead><tr><th>Name</th><th>Type</th><th></th></tr></thead><tbody>{{range .Credentials}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td class="actions">{{if or (eq .Kind "webhook") (eq .Kind "hermes") (eq .Kind "openclaw")}}<a class="btn secondary icon-btn" href="/ui/clients/{{.ID}}/log" title="Delivery log" aria-label="Delivery log"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3.5 3.5h9M3.5 6.5h9M3.5 9.5h6"/><path d="M11.5 12.5h1M3.5 12.5h5"/></svg></a>{{end}}<button type="button" class="secondary icon-btn edit-credential" data-id="{{.ID}}" data-kind="{{.Kind}}" data-name="{{.Name}}" data-admin="{{if .Admin}}1{{end}}" data-roles="{{.RolesJSON}}" data-inbox="{{.InboxID}}" data-role="{{.Role}}" data-url="{{.URL}}" data-mode="{{.Mode}}" data-auth="{{.AuthMode}}" title="Client settings" aria-label="Client settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></button><button type="button" class="secondary icon-btn danger open-delete-client" data-kind="{{if eq .Kind "hermes"}}hermes{{else if eq .Kind "openclaw"}}openclaw{{else if eq .Kind "webhook"}}webhooks{{else}}keys{{end}}" data-id="{{.ID}}" data-name="{{.Name}}" data-type="{{.Type}}" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No clients yet.</p>{{end}}</section>
<section class="card"><div class="card-head"><h2>Domains</h2><button type="button" id="add-domain">Add Domain</button></div>{{if .Domains}}<div class="table-wrap"><table class="domains-table"><thead><tr><th>Domain</th><th>Catch-all</th><th>Sending</th><th>Receiving</th><th></th></tr></thead><tbody>{{range .Domains}}{{$d := .}}<tr><td><b>{{.Name}}</b>{{if .ParentDomainID}} <span class="subdomain-tag" title="Subdomain of {{.ParentDomain}}"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2.2 4.6 7h1.8L4 10.6h8L9.6 7h1.8L8 2.2Z"/><path d="M8 10.6V14"/><path d="M5.8 14h4.4"/></svg></span>{{end}}</td><td>{{if .CatchAllInboxID}}<button type="button" class="cell-link domain-catchall-link open-domain-dialog" data-domain="{{.ID}}" data-kind="catchall" title="{{index $.InboxAddr .CatchAllInboxID}}">{{index $.InboxAddr .CatchAllInboxID}}</button>{{else}}<button type="button" class="secondary btn-sm cell-edit domain-catchall-add open-domain-dialog" data-domain="{{.ID}}" data-kind="catchall">Add</button>{{end}}</td><td><button type="button" class="{{if .SendingProvider}}secondary{{else}}amber{{end}} btn-sm cell-edit domain-provider-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="sending">{{if .SendingProvider}}{{if .SendingInheritedFrom}}<span class="inherited">(inherited)</span>{{else}}{{index $.DomainSendingLabel .ID}}{{end}}{{else}}Add{{end}}</button></td><td><button type="button" class="{{if .ReceivingProvider}}secondary{{else}}amber{{end}} btn-sm cell-edit domain-provider-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="receiving">{{if .ReceivingProvider}}{{if .ReceivingInheritedFrom}}<span class="inherited">(inherited)</span>{{else}}{{index $.DomainReceivingLabel .ID}}{{end}}{{else}}Add{{end}}</button></td><td><span class="domain-actions"><a class="btn secondary icon-btn" href="/ui/domains/{{.ID}}/sending/deliveries" title="Activity log" aria-label="Activity log"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="2.5" width="9" height="11" rx="1.5"/><path d="M5.5 5.5h5M5.5 8h5M5.5 10.5h3"/></svg></a><button type="button" class="secondary icon-btn danger open-delete-domain" data-domain="{{.ID}}" data-name="{{.Name}}" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></span></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">Add your first domain.</p>{{end}}</section></div>
<section class="card"><h2>Recent messages</h2><form method="get" action="/" class="search-form"><input name="q" value="" placeholder="Search mail"><button>Search</button></form>{{if .Messages}}<div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Direction</th><th>From</th><th>To</th><th>Subject</th><th>Client</th><th></th></tr></thead><tbody>{{range .Messages}}<tr><td style="white-space:nowrap">{{localDateTime .CreatedAt}}</td><td>{{if .Blocked}}<span class="pill amber">Blocked</span>{{else if eq .Direction "outbound"}}<span class="pill">Sent</span>{{else}}<span class="pill">Received</span>{{end}}</td><td>{{if .From.Address}}{{.From.Address}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Subject}}{{.Subject}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if eq .Client "Control"}}<span class="pill">Control</span>{{else if .Client}}{{.Client}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if or .Blocked .Approval (not .ID)}}<span class="muted">—</span>{{else}}<a href="/ui/messages/{{.ID}}">Open</a>{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No messages yet.</p>{{end}}</section>
</div>

<dialog id="key-dialog"><h2 id="key-dialog-title">Add client</h2><form method="post" action="/ui/keys" id="key-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><label>Type</label><select name="type" id="key-type"><option value="api">API key</option><option value="hermes">Hermes relay connection</option><option value="openclaw">OpenClaw agent connector</option><option value="webhook">Webhook delivery</option></select><label>Name</label><input name="name" placeholder="Hermes EA" required><fieldset class="key-fields" data-type="api" style="border:0;padding:0;margin:0"><label><input type="checkbox" name="admin" value="1"> Account Admin Key (Full permission on all mailboxes and can create and delete mailboxes)</label><fieldset id="key-matrix" style="border:0;padding:0;margin:0">{{if .Inboxes}}<table class="key-matrix"><thead><tr><th>Inbox</th><th><span class="muted">Set all:</span> <div class="seg" data-set-scope="all"><button type="button" data-set-role="">None</button><button type="button" data-set-role="read">Read</button><button type="button" data-set-role="assistant">Assistant</button><button type="button" data-set-role="owner">Owner</button></div></th></tr></thead>{{range .Domains}}{{$d := .}}{{if index $.DomainInboxes $d.ID}}<tbody data-domain="{{$d.ID}}"><tr class="domain-row"><td><b>{{$d.Name}}</b></td><td><div class="seg" data-set-scope="{{$d.ID}}"><button type="button" data-set-role="" data-domain="{{$d.ID}}">None</button><button type="button" data-set-role="read" data-domain="{{$d.ID}}">Read</button><button type="button" data-set-role="assistant" data-domain="{{$d.ID}}">Assistant</button><button type="button" data-set-role="owner" data-domain="{{$d.ID}}">Owner</button></div></td></tr>{{range index $.DomainInboxes $d.ID}}<tr><td class="domain-inbox">{{.Address}}</td><td><div class="seg"><input type="radio" id="role_{{.ID}}_none" name="role_{{.ID}}" value="" checked><label for="role_{{.ID}}_none">None</label><input type="radio" id="role_{{.ID}}_read" name="role_{{.ID}}" value="read"><label for="role_{{.ID}}_read">Read</label><input type="radio" id="role_{{.ID}}_assistant" name="role_{{.ID}}" value="assistant"><label for="role_{{.ID}}_assistant">Assistant</label><input type="radio" id="role_{{.ID}}_owner" name="role_{{.ID}}" value="owner"><label for="role_{{.ID}}_owner">Owner</label></div></td></tr>{{end}}</tbody>{{end}}{{end}}</table>{{else}}<p class="muted">Create an inbox first to grant mailbox access.</p>{{end}}<table class="role-legend"><thead><tr><th>Role</th><th>Grants</th></tr></thead><tbody><tr><td>Read</td><td>Read messages/threads, search, download attachments.</td></tr><tr><td>Assistant</td><td>Read plus delete messages and create/edit drafts. Cannot send.</td></tr><tr><td>Owner</td><td>Full mailbox access: read, delete, send.</td></tr></tbody></table></fieldset></fieldset><fieldset class="key-fields" data-type="hermes" style="border:0;padding:0;margin:0"><label class="connector-inbox-label">Inbox</label><select class="connector-inbox-select" name="inbox">{{range .Inboxes}}<option value="{{.ID}}" data-allowlist="{{if .SenderRestricted}}1{{end}}">{{.Address}}</option>{{end}}</select><label>Outbound authority</label><select name="role"><option value="owner">Owner — relay sends directly</option><option value="assistant">Assistant — relay drafts and requests approval</option></select><div class="banner" id="key-hermes-warning" hidden style="background:#fdecef;border-color:#e0a0aa;color:#b00020"><b>This inbox has no allow list.</b> The Hermes agent will respond to anyone who emails this inbox. We strongly recommend you set an allow list of permitted senders before creating a Hermes relay connection to this mailbox. Click edit next to the mailbox to configure an allow list.</div><label id="key-hermes-ack-row" hidden style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="ack" value="1" id="key-hermes-ack" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>I understand the risk of my agent responding to anyone who emails it</span></label></fieldset><fieldset class="key-fields" data-type="openclaw" style="border:0;padding:0;margin:0"><label class="connector-inbox-label">Inbox</label><select class="connector-inbox-select" name="inbox">{{range .Inboxes}}<option value="{{.ID}}" data-allowlist="{{if .SenderRestricted}}1{{end}}">{{.Address}}</option>{{end}}</select><label>Outbound authority</label><select name="role"><option value="owner">Owner — agent sends directly</option><option value="assistant">Assistant — agent drafts and requests approval</option></select><label>Setup method</label><select name="setup"><option value="code">One-time code — run openclaw channels add (recommended)</option><option value="manual">Manual config block — for air-gapped installs</option></select><div class="banner" id="key-openclaw-warning" hidden style="background:#fdecef;border-color:#e0a0aa;color:#b00020"><b>This inbox has no allow list.</b> Your OpenClaw agent will respond to anyone who emails this inbox. We strongly recommend you set an allow list of permitted senders before creating an OpenClaw connector to this mailbox. Click edit next to the mailbox to configure an allow list.</div><label id="key-openclaw-ack-row" hidden style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="ack" value="1" id="key-openclaw-ack" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>I understand the risk of my agent responding to anyone who emails it</span></label></fieldset><fieldset class="key-fields" data-type="webhook" style="border:0;padding:0;margin:0"><label class="connector-inbox-label">Inbox</label><select class="connector-inbox-select" name="inbox">{{range .Inboxes}}<option value="{{.ID}}">{{.Address}}</option>{{end}}</select><label>Destination URL</label><input name="url" type="url" placeholder="https://example.com/hook" autocomplete="off"><label>Payload</label><select name="mode"><option value="notify">Notify — small JSON with the message id</option><option value="forward">Forward — full raw MIME</option></select><label>Authentication</label><select name="auth"><option value="signature">Signature — signed HMAC-SHA256 header</option><option value="bearer">Bearer — static token</option></select></fieldset><div class="error" id="key-error" hidden></div><div class="dialog-actions"><button type="button" class="amber" id="key-rotate" hidden>Rotate Key</button><button type="button" class="secondary" id="key-cancel">Cancel</button><button id="key-submit">Add Client</button></div></form><div id="key-result" hidden><h3 id="key-result-title"></h3><p class="muted" id="key-result-label"></p><div class="secret"><pre id="key-result-secret"></pre></div><p class="copy-note" id="key-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the key above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="key-copy">Copy</button><button type="button" id="key-done">Done</button></div></div></dialog>
<dialog id="add-domain-dialog"><form method="post" action="/ui/domains" data-domain-names="{{.DomainNamesCSV}}"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Domain</label><input name="name" id="add-domain-name" placeholder="example.com" required aria-describedby="add-domain-hint"><p class="muted small" id="add-domain-hint">Enter the bare domain (<code>example.com</code>). Add the whole domain even if you only use a few addresses. Configure sending and receiving from the domain's row in the Domains list.</p><div id="add-domain-inherit" hidden><p class="muted small"><b><span class="add-domain-parent"></span></b> is already configured. Reuse its connectors for this subdomain, or untick to configure it separately.</p><label class="inherit-option"><input type="checkbox" name="inherit_receiving" value="1" checked> <span>Use <b class="add-domain-parent"></b>&rsquo;s receiving configuration</span></label><label class="inherit-option"><input type="checkbox" name="inherit_sending" value="1" checked> <span>Use <b class="add-domain-parent"></b>&rsquo;s sending configuration</span></label><input type="hidden" name="inherit_controls" id="add-domain-inherit-controls" value=""></div><div class="dialog-actions"><button type="button" class="secondary" id="add-domain-cancel">Cancel</button><button>Add Domain</button></div></form></dialog>
<dialog id="domain-delete-dialog" class="domain-dialog"><h2>Delete domain</h2><p class="muted">This permanently deletes <b id="domain-delete-name"></b> and everything it owns — all of its inboxes, messages and attachments. This cannot be undone.</p><form method="post" id="domain-delete-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Type the domain name to confirm</label><input name="confirm" id="domain-delete-input" autocomplete="off" required><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button class="secondary danger" id="domain-delete-submit" disabled>Delete domain</button></div></form></dialog>
<dialog id="inbox-delete-dialog" class="domain-dialog"><h2>Delete inbox</h2><p class="muted">This permanently deletes <b id="inbox-delete-address"></b> and all of its messages and attachments. This cannot be undone.</p><form method="post" id="inbox-delete-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Type the email address to confirm</label><input name="confirm" id="inbox-delete-input" autocomplete="off" required><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button class="secondary danger" id="inbox-delete-submit" disabled>Delete inbox</button></div></form></dialog>
<dialog id="client-delete-dialog" class="domain-dialog"><h2>Delete client</h2><p class="muted">This permanently deletes <b id="client-delete-label"></b> and revokes its access. This cannot be undone.</p><form method="post" id="client-delete-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button class="secondary danger" id="client-delete-submit">Delete client</button></div></form></dialog>
<dialog id="inbox-dialog"><h2>Add inbox</h2><div class="dialog-tabs" role="tablist" data-inbox-tabs><button type="button" class="dialog-tab active" role="tab" aria-selected="true" data-inbox-tab="basic">Basic</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="allow">Allow list</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="approver">Approver</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="quota">Quota</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="aliases">Aliases</button></div><form method="post" action="/ui/inboxes"><input type="hidden" name="_csrf" value="{{.CSRF}}"><section class="dialog-panel" role="tabpanel" data-inbox-panel="basic"><label>Email address</label><div class="email-field"><input name="local" placeholder="hermes" required><span class="at">@</span><select name="domain" required>{{range .Domains}}<option value="{{.ID}}" data-mx="{{if index $.DomainIsMX .ID}}1{{end}}">{{.Name}}</option>{{end}}</select></div><label>Display Name</label><input name="display" placeholder="Hermes"></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="allow" hidden><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="sender_restricted" value="1" id="inbox-add-sender-restricted" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Block senders to this inbox except the allow list below</span></label><div id="inbox-add-sender-section" hidden><p class="muted" id="inbox-add-sender-note">Only these From addresses are accepted. The From header can be spoofed, so this is a filter, not proof of identity.</p><ul id="inbox-add-sender-list" class="slist"><li class="empty">Add an address below to allow it to email this inbox.</li></ul><div class="row"><input id="inbox-add-sender-input" type="text" placeholder="someone@example.com"><button type="button" class="secondary btn-narrow" id="inbox-add-sender-add">Add</button></div><p class="muted small" id="inbox-add-sender-hint">Use <code>*@example.com</code> to allow any sender at a domain, or <code>*@*.example.com</code> for its subdomains. The approver is always allowed.</p></div><div id="inbox-add-require-auth-section" hidden><h3 class="section-head">MX delivery</h3><p class="muted small">This domain receives mail by direct SMTP (MX). Require authenticated senders (SPF, DKIM or DMARC pass) in addition to the allow list.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="require_authenticated" value="1" id="inbox-add-require-auth" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Require an authenticated sender</span></label></div></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="approver" hidden><label>Approver email</label><input name="approver_email" id="inbox-add-approver-email" placeholder="Optional — approves draft sends by email"><p class="muted small" id="inbox-add-approver-note">When set, this address will receive approval requests for emails requested to send by clients with Assistant permission.</p></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="quota" hidden><p class="muted">Storage is limited at the account level.</p><dl class="dialog-usage"><dt>This inbox</dt><dd>No usage yet — new inbox.</dd><dt>Account used</dt><dd>{{filesize .Account.StorageUsedBytes}} of {{filesize .Account.StorageQuotaBytes}}</dd></dl><p class="muted small">Per-inbox usage appears here after the inbox receives mail.</p></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="aliases" hidden><h3 class="section-head">Managed aliases</h3><p class="muted">Alternate addresses that deliver to this inbox and that this inbox can send from. An alias may be on any domain in this account. Each alias has a sender name and an email address.</p><button type="button" class="secondary btn-narrow" id="inbox-add-alias-add">Add alias</button><ul id="inbox-add-alias-list" class="slist aliases"><li class="empty">No aliases.</li></ul><h3 class="section-head">External sending aliases</h3><p class="muted">Send-only addresses. Mail sent to them stays with that provider, not MailMoose. Save the inbox first, then add external sending aliases from its Edit dialog.</p><h3 class="section-head">Primary / Default Address</h3><select name="default_sender" id="inbox-add-default-sender"><option value="">Primary address</option></select></section><div class="dialog-actions"><button type="button" class="secondary" id="inbox-cancel">Cancel</button><button>Add Inbox</button></div></form></dialog>
<dialog id="inbox-edit-dialog"><h2>Edit inbox</h2><div class="dialog-tabs" role="tablist" data-inbox-tabs><button type="button" class="dialog-tab active" role="tab" aria-selected="true" data-inbox-tab="basic">Basic</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="allow">Allow list</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="approver">Approver</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="quota">Quota</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="aliases">Aliases</button><button type="button" class="dialog-tab" role="tab" aria-selected="false" data-inbox-tab="connectors">Connectors</button></div><form method="post" id="inbox-edit-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><section class="dialog-panel" role="tabpanel" data-inbox-panel="basic"><label>Display name</label><input name="display"><label>Email address</label><input id="inbox-edit-address" value="" disabled></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="allow" hidden><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="sender_restricted" value="1" id="inbox-sender-restricted" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Block senders to this inbox except the allow list below</span></label><div id="inbox-sender-section" hidden><p class="muted" id="inbox-sender-note">Only these From addresses are accepted. The From header can be spoofed, so this is a filter, not proof of identity.</p><ul id="inbox-sender-list" class="slist"><li class="empty">Add an address below to allow it to email this inbox.</li></ul><div class="row"><input id="inbox-sender-input" type="text" placeholder="someone@example.com"><button type="button" class="secondary btn-narrow" id="inbox-sender-add">Add</button></div><p class="muted small" id="inbox-sender-hint">Use <code>*@example.com</code> to allow any sender at a domain, or <code>*@*.example.com</code> for its subdomains. The approver above is always allowed.</p></div><div id="inbox-edit-require-auth-section" hidden><h3 class="section-head">MX delivery</h3><p class="muted small">This domain receives mail by direct SMTP (MX). Require authenticated senders (SPF, DKIM or DMARC pass) in addition to the allow list.</p><label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="require_authenticated" value="1" id="inbox-require-auth" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Require an authenticated sender</span></label></div></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="approver" hidden><label>Approver email</label><input name="approver_email" id="inbox-edit-approver-email" placeholder="Optional — approves draft sends by email"><p class="muted small" id="inbox-approver-note">When set, this address will receive approval requests for emails requested to send by clients with Assistant permission.</p></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="quota" hidden><p class="muted">Storage is limited at the account level.</p><dl class="dialog-usage"><dt>This inbox</dt><dd id="inbox-edit-usage">—</dd><dt>Account used</dt><dd>{{filesize .Account.StorageUsedBytes}} of {{filesize .Account.StorageQuotaBytes}}</dd></dl><p class="muted small">Per-inbox quota limits are not configurable yet.</p></section><section class="dialog-panel" role="tabpanel" data-inbox-panel="aliases" hidden><h3 class="section-head">Managed aliases</h3><p class="muted">Alternate addresses that deliver to this inbox and that this inbox can send from. An alias may be on any domain in this account. Each alias has a sender name and an email address.</p><button type="button" class="secondary btn-narrow" id="inbox-alias-add">Add alias</button><ul id="inbox-alias-list" class="slist aliases"><li class="empty">No aliases.</li></ul><h3 class="section-head">External sending aliases</h3><p class="muted">Send-only aliases. Use one to keep sending from an address you already own (Gmail, Outlook, …). Mail sent to that address stays with that provider, not MailMoose. Each address has its own sending configuration, set separately.</p><div id="inbox-external-alias-section"><button type="button" class="secondary btn-narrow" id="inbox-external-alias-add">Add external alias</button><ul id="inbox-external-alias-list" class="slist aliases external"><li class="empty">No external sending aliases.</li></ul></div><h3 class="section-head">Primary / Default Address</h3><select name="default_sender" id="inbox-default-sender"><option value="">Primary address</option></select></section></form><section class="dialog-panel" role="tabpanel" data-inbox-panel="connectors" hidden><div class="card-head"><div><h3 class="section-head">Connectors</h3><p class="muted small">Hermes Relay and Webhook connections attached to this inbox.</p></div><button type="button" class="secondary btn-sm add-connector" id="inbox-connector-add" data-inbox="">Add Connector</button></div><div id="inbox-connectors-list" class="connector-list"></div><div id="inbox-connector-editor" class="connector-editor connector-view-hidden"></div></section><div class="dialog-actions"><button type="button" class="secondary danger open-delete-inbox" id="inbox-edit-delete" data-id="" data-address="">Delete Inbox</button><button type="button" class="secondary" id="inbox-edit-cancel">Cancel</button><button type="submit" form="inbox-edit-form" id="inbox-edit-save">Save</button></div></dialog>
<dialog id="alias-dialog"><h2 id="alias-dialog-title">Add alias</h2><div class="error" id="alias-error" hidden></div><label>Name</label><input id="alias-name" type="text" placeholder="Acme Sales" maxlength="128" required><label>Email address</label><div class="email-field"><input id="alias-local" type="text" placeholder="sales" required><span class="at">@</span><select id="alias-domain">{{range .Domains}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select></div><div class="dialog-actions"><button type="button" class="secondary" id="alias-cancel">Cancel</button><button type="button" id="alias-save">Add</button></div></dialog>
<dialog id="external-alias-dialog"><h2 id="external-alias-dialog-title">Add external sending alias</h2><form method="post" id="external-alias-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><div class="error" id="external-alias-error" hidden></div><label>Sender name</label><input name="external_alias_name" id="external-alias-name" type="text" placeholder="Agent" maxlength="128"><label>Email address</label><input name="external_alias" id="external-alias-address" type="email" placeholder="agent@gmail.com" required><p class="muted small">Send-only address. Mail sent here stays with that provider, not MailMoose. Configure its sending settings after saving.</p><div class="dialog-actions"><button type="button" class="secondary" id="external-alias-cancel">Cancel</button><button type="submit" id="external-alias-save">Add external alias</button></div></form></dialog>
{{range .Domains}}{{$d := .}}{{$sel := index $.DomainSendingSelected .ID}}<dialog id="domain-sending-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "sending")}} data-open="1"{{end}}><h2>Sending · {{.Name}}</h2><form id="domain-sending-form-{{$d.ID}}" method="post" action="/ui/domains/{{$d.ID}}/sending" class="cfg-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><label>Provider</label><select name="provider" class="provider-select"><option value="">Select a provider…</option>{{if $d.ParentDomainID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{$d.ParentDomain}})</option>{{else if index $.DomainParentCandidate $d.ID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{index $.DomainParentCandidate $d.ID}})</option>{{end}}{{range index $.DomainSendingEditors $d.ID}}<option value="{{.Provider}}"{{if eq .Provider $sel}} selected{{end}} data-next="{{if .Generated}}1{{end}}">{{.ProviderLabel}}</option>{{end}}</select><p class="muted provider-hint"{{if $sel}} hidden{{end}}>Choose a provider to configure sending.</p>{{range index $.DomainSendingEditors $d.ID}}{{$e := .}}<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>{{if .Error}}<div class="error">{{.Error}}</div>{{end}}{{if .KeepSecrets}}<p class="muted">Saving {{.ProviderLabel}} updates this domain's sending configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted">Saving {{.ProviderLabel}} replaces this domain's sending configuration. Required secrets must be entered.</p>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}</div>{{end}}</form><div class="dialog-actions">{{if $d.SendingProvider}}<div class="dialog-danger"><form method="post" action="/ui/domains/{{$d.ID}}/sending/clear" data-confirm="Remove sending configuration for this domain? Mail will queue until a provider is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove sending</button></form></div>{{end}}<button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="domain-sending-form-{{$d.ID}}" data-save-provider{{if not $sel}} disabled{{end}}>Save</button></div></dialog>
{{end}}{{range .Domains}}{{$d := .}}{{$sel := index $.DomainReceivingSelected .ID}}
<dialog id="domain-receiving-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "receiving")}} data-open="1"{{end}}>
<h2>Receiving · {{.Name}}</h2>
<form id="domain-receiving-form-{{$d.ID}}" method="post" action="/ui/domains/{{$d.ID}}/receiving" class="cfg-form" autocomplete="off">
<input type="hidden" name="_csrf" value="{{$.CSRF}}">
<label>Provider</label>
<select name="provider" class="provider-select"><option value="">Select a provider…</option>{{if $d.ParentDomainID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{$d.ParentDomain}})</option>{{else if index $.DomainParentCandidate $d.ID}}<option value="inherited"{{if eq $sel "inherited"}} selected{{end}}>Inherited (from {{index $.DomainParentCandidate $d.ID}})</option>{{end}}{{range index $.DomainReceivingEditors $d.ID}}<option value="{{.Provider}}"{{if eq .Provider $sel}} selected{{end}} data-next="{{if .Generated}}1{{end}}">{{.ProviderLabel}}</option>{{end}}</select>
<p class="muted provider-hint"{{if $sel}} hidden{{end}}>Choose a provider to configure receiving.</p>
{{range index $.DomainReceivingEditors $d.ID}}{{$e := .}}{{if not .Generated}}
<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if .WebhookURL}}<p class="muted">Register this webhook URL with {{.ProviderLabel}} before saving:</p><div class="secret"><pre class="setup-webhook-url">{{.WebhookURL}}</pre></div><p class="copy-note setup-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the URL above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary setup-copy">Copy webhook URL</button></div>{{end}}
{{if .Steps}}<ol class="steps">{{range .Steps}}<li>{{.}}</li>{{end}}</ol>{{end}}
{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}
{{if .KeepSecrets}}<p class="muted small">Saving updates this domain's receiving configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted small">Saving replaces this domain's receiving configuration. Required secrets must be entered.</p>{{end}}
</div>
{{end}}{{end}}
</form>
{{with index $.DialMXSetup $d.ID}}<section class="dialmx-setup">
<h3 class="section-head">Dial MX key &amp; DNS</h3>
{{if .ReceiverURLs}}<p class="muted small">Receiver URLs: <code>{{.ReceiverURLs}}</code></p>{{end}}
<p><b>Publish this TXT record at <code>_mailmoose-mx.{{$d.Name}}</code>:</b></p>
<pre class="dialmx-txt">{{.TXT}}</pre>
<p class="muted small">Key ID: <code>{{.KeyID}}</code> · Public key: <code>{{.PublicKey}}</code></p>
{{if .Statuses}}<dl class="dialmx-status">{{range .Statuses}}<dt>{{.ReceiverURL}}</dt><dd>{{if dialmxReady .}}<span class="pill">ready</span>{{if .SMTPHostname}} · point MX at <code>{{.SMTPHostname}}</code>{{end}}{{if dialmxExpiry .}} · rekey in {{dialmxExpiry .}}{{end}}{{else}}<span class="pill amber">rejected</span>{{if dialmxReason .}} · {{dialmxReason .}}{{end}}{{end}}</dd>{{end}}</dl>{{else}}<p class="muted small">No live receiver status yet. Save the configuration, then DNS and receiver authorization are confirmed automatically.</p>{{end}}
<p class="copy-note">After regenerating the key, replace this TXT record too. Authorization is re-established within about five minutes.</p>
</section>{{end}}
<div class="dialog-actions">{{if or $d.ReceivingProvider (index $.DialMXSetup $d.ID)}}<div class="dialog-danger">{{if index $.DomainReceivingRegenerate $d.ID}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/regenerate" data-confirm="Regenerate the Worker secret? The current Worker stops working until you paste the new code."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="amber">Regenerate secret</button></form>{{end}}{{with index $.DialMXSetup $d.ID}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/regenerate"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary">Regenerate key</button></form>{{end}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/clear" data-confirm="Remove receiving configuration for this domain? It will stop accepting mail until a receive path is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove receiving</button></form></div>{{end}}<button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="domain-receiving-form-{{$d.ID}}" data-save-provider{{if not $sel}} disabled{{end}}>Save</button></div>
</dialog>
{{end}}
{{range .Domains}}{{$d := .}}<dialog id="domain-catchall-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "catchall")}} data-open="1"{{end}}><h2>Catch-all · {{.Name}}</h2><form method="post" action="/ui/domains/{{.ID}}/catchall"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><p class="muted">Mail sent to an unknown address on this domain is delivered to this inbox. Only inboxes on {{.Name}} can be selected.</p><label>Catch-all inbox</label><select name="inbox"><option value="">No catch-all</option>{{range index $.DomainInboxes $d.ID}}<option value="{{.ID}}"{{if eq .ID $d.CatchAllInboxID}} selected{{end}}>{{.Address}}</option>{{end}}</select><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button>Save catch-all</button></div></form></dialog>
{{end}}
` + externalAliasConnectorDialogs

const inboxTableTemplate = `{{define "inboxes-table"}}<div class="table-wrap"><table class="dense"><thead><tr><th>Name</th><th></th><th class="hcenter">Unread</th><th class="hcenter">Pending send</th><th>Address</th>{{if $.Principal.Admin}}<th>Connectors</th>{{end}}<th>Size</th>{{if $.Principal.Admin}}<th></th>{{end}}</tr></thead><tbody>{{range .Inboxes}}{{$inbox := .}}<tr class="row-link" data-href="/ui/inboxes/{{.ID}}"><td><a href="/ui/inboxes/{{.ID}}">{{if .DisplayName}}{{.DisplayName}}{{else}}<span class="muted">—</span>{{end}}</a></td><td class="inbox-flags" style="white-space:nowrap">{{$sp := index $.InboxSendingReady .ID}}{{$rp := index $.DomainReceivingReady .DomainID}}{{if or (not $sp) (not $rp)}}<span class="issue-dot" style="color:#b3261e;vertical-align:middle"{{if and (not $sp) (not $rp)}} title="No Sender Configured for Inbox&#10;No Receiver Configured for Domain" aria-label="No Sender Configured for Inbox, No Receiver Configured for Domain"{{else if not $sp}} title="No Sender Configured for Inbox" aria-label="No Sender Configured for Inbox"{{else}} title="No Receiver Configured for Domain" aria-label="No Receiver Configured for Domain"{{end}}><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="8" r="6"/><path d="m5.9 5.9 4.2 4.2M10.1 5.9l-4.2 4.2"/></svg></span>{{end}}{{if .SenderRestricted}} <span title="Sender allow list:&#10;{{range $i, $a := .AllowedSenders}}{{if $i}}&#10;{{end}}{{$a}}{{end}}{{if not .AllowedSenders}}None{{end}}&#10;&#10;Matches the From address, which can be spoofed" aria-label="Restricted to allowed senders" style="color:#5f6368;vertical-align:middle"><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="7" width="9" height="6.5" rx="1.2"/><path d="M5.5 7V5.5a2.5 2.5 0 0 1 5 0V7"/></svg></span>{{end}}{{if .Aliases}} <span title="Aliases:&#10;{{range $i, $a := .Aliases}}{{if $i}}&#10;{{end}}{{$a}}{{end}}" aria-label="Has aliases" style="color:#5f6368;vertical-align:middle"><svg viewBox="0 0 16 16" width="14.4" height="14.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="6.2" cy="5" r="2.3"/><path d="M10.8 13.4v-1a2.6 2.6 0 0 0-2.6-2.6H4.2a2.6 2.6 0 0 0-2.6 2.6v1"/><path d="M14.2 13.4v-1a2.6 2.6 0 0 0-1.9-2.5"/><path d="M10 2.6a2.3 2.3 0 0 1 0 4.6"/></svg></span>{{end}}</td><td style="white-space:nowrap;text-align:center">{{if index $.Unread .ID}}<span class="pill unread-pill">{{index $.Unread .ID}}</span>{{else}}<span class="muted">—</span>{{end}}</td><td style="white-space:nowrap;text-align:center">{{if index $.DraftCounts .ID}}<a class="pill pending-pill" href="/ui/inboxes/{{.ID}}/drafts" title="Drafts awaiting approval to send">{{index $.DraftCounts .ID}}</a>{{else}}<span class="muted">—</span>{{end}}</td><td style="white-space:nowrap">{{.Address}}</td>{{if $.Principal.Admin}}<td class="inbox-connectors"><div class="connector-chips"><button type="button" class="secondary btn-sm connector-add add-connector" data-inbox="{{$inbox.ID}}" title="Add connector" aria-label="Add connector">+</button>{{range index $.InboxConnectors .ID}}<button type="button" class="secondary btn-sm connector-chip open-inbox-connector" data-inbox="{{$inbox.ID}}" data-connector="{{.ID}}" title="{{if eq .Kind "hermes"}}Hermes Relay: {{.Name}}{{else if eq .Kind "openclaw"}}OpenClaw: {{.Name}}{{else}}Webhook: {{.Name}}&#10;{{shortURL .URL}}{{end}}" aria-label="{{if eq .Kind "hermes"}}Hermes Relay: {{.Name}}{{else if eq .Kind "openclaw"}}OpenClaw: {{.Name}}{{else}}Webhook: {{.Name}}, {{.URL}}{{end}}">{{if eq .Kind "hermes"}}<img class="connector-brand-image" alt="" aria-hidden="true" src="{{asset "hermes-connector.png"}}">{{else if eq .Kind "openclaw"}}<img class="connector-brand-image connector-brand-openclaw" alt="" aria-hidden="true" src="{{asset "openclaw-connector.png"}}">{{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 16.98h-5.99c-1.1 0-1.95.94-2.48 1.9A4 4 0 0 1 2 17c.01-.7.2-1.4.57-2"/><path d="m6 17 3.13-5.78c.53-.97.1-2.18-.5-3.1a4 4 0 1 1 6.89-4.06"/><path d="m12 6 3.13 5.73C15.66 12.7 16.9 13 18 13a4 4 0 0 1 0 8"/></svg>{{end}}</button>{{end}}</div></td>{{end}}<td style="white-space:nowrap">{{filesize (index $.MailboxSizes .ID)}}</td>{{if $.Principal.Admin}}<td class="actions" style="white-space:nowrap"><button type="button" class="secondary icon-btn edit-inbox" data-id="{{.ID}}" data-name="{{.DisplayName}}" data-address="{{.Address}}" data-allowed="{{join .AllowedSenders ","}}" data-restricted="{{if .SenderRestricted}}1{{end}}" data-require-auth="{{if .RequireAuthenticated}}1{{end}}" data-mx="{{if index $.DomainIsMX .DomainID}}1{{end}}" data-approver-email="{{.ApproverEmail}}" data-aliases="{{join .Aliases ","}}" data-alias-names="{{aliasNames .Aliases .AliasNames}}" data-external-aliases="{{externalAliases .ExternalAliases}}" data-connectors="{{connectorsJSON (index $.InboxConnectors .ID)}}" data-default-sender="{{.DefaultSender}}" data-usage="{{filesize (index $.MailboxSizes .ID)}}" title="Inbox settings" aria-label="Inbox settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></button><button type="button" class="secondary icon-btn danger open-delete-inbox" data-id="{{.ID}}" data-address="{{.Address}}" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></td>{{end}}</tr>{{end}}</tbody></table></div>{{end}}`

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
	conns, _ := s.Service.Store.ListHermesConnections(ctx, p.AccountID)
	webhooks, _ := s.Service.Store.ListWebhookClients(ctx, p.AccountID)
	connectorViews := credentialViews(nil, conns, webhooks)
	inboxConnectors := make(map[string][]credentialView, len(boxes))
	for _, connector := range connectorViews {
		inboxConnectors[connector.InboxID] = append(inboxConnectors[connector.InboxID], connector)
	}
	var msgs []model.Message
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		msgs, _ = s.Service.Store.SearchMessages(ctx, p, q, "", 100)
	} else {
		activity, _ := s.Service.Store.ListAccountLog(ctx, p.AccountID, 100)
		msgs = dashboardActivityRows(activity)
	}
	sendingReady, receivingReady, inboxSendingReady := inboxReadiness(domains, boxes)
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
		if cfg, err := s.Service.Store.ResolveDomainReceivingConfig(ctx, p.AccountID, d.ID, "dialmx"); err == nil {
			if values, err := s.Service.DecryptDomainReceivingConfig(cfg); err == nil {
				urls, _ = values["receiver_urls"].(string)
			}
		}
		dialMXSetup[d.ID] = &dialMXSetupView{
			KeyID:        credential.KeyID,
			PublicKey:    credential.PublicKey,
			TXT:          mxwire.DomainTXT(credential.KeyID, decodePublicKey(credential.PublicKey)),
			ReceiverURLs: urls,
			Statuses:     s.dialMXStatuses(d.Name),
		}
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
	unread, _ := s.Service.Store.UnreadCounts(ctx, p)
	if unread == nil {
		unread = map[string]int{}
	}
	draftCounts, _ := s.Service.Store.PendingDraftCountsByInbox(ctx, p)
	if draftCounts == nil {
		draftCounts = map[string]int{}
	}
	mailboxSizes, _ := s.Service.Store.MessageSizesByInbox(ctx, p)
	if mailboxSizes == nil {
		mailboxSizes = map[string]int64{}
	}

	// The dialog to open is named in the query so a provider switch or a
	// post-save error can reload the dashboard with the right editor visible. A
	// foreign or stale domain id is dropped rather than trusted.
	if openID != "" {
		if _, err := s.Service.Store.GetDomain(ctx, p.AccountID, openID); err != nil {
			openID, openKind = "", ""
		}
	}
	// Secondary settings flows can return to a specific inbox tab. Existing
	// external-alias flows omit inbox_tab and therefore continue to return to
	// Aliases; connector actions explicitly return to Connectors.
	inboxOpenID := strings.TrimSpace(r.URL.Query().Get("inbox"))
	inboxOpenTab := strings.TrimSpace(r.URL.Query().Get("inbox_tab"))
	inboxOpenConnectorID := strings.TrimSpace(r.URL.Query().Get("connector"))
	if inboxOpenTab == "" {
		inboxOpenTab = "aliases"
	}
	if inboxOpenTab != "aliases" && inboxOpenTab != "connectors" {
		inboxOpenTab = "aliases"
	}
	if inboxOpenID != "" {
		if _, err := s.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxOpenID); err != nil {
			inboxOpenID, inboxOpenTab, inboxOpenConnectorID = "", "", ""
		}
	}
	if inboxOpenID != "" && inboxOpenTab == "connectors" && inboxOpenConnectorID != "" {
		found := false
		for _, connector := range inboxConnectors[inboxOpenID] {
			if connector.ID == inboxOpenConnectorID {
				found = true
				break
			}
		}
		if !found {
			inboxOpenConnectorID = ""
		}
	} else if inboxOpenTab != "connectors" {
		inboxOpenConnectorID = ""
	}

	sendingEditors := make(map[string][]*domainEditorView, len(domains))
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
		editors := s.domainSendingEditors(ctx, p.AccountID, d.ID)
		for _, e := range editors {
			if e.Provider == sendProvider {
				e.Selected = true
				sendingLabel[d.ID] = e.ProviderLabel
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
				if e.Provider == "mx" {
					receivingLabel[d.ID] = "MX"
				} else {
					receivingLabel[d.ID] = e.ProviderLabel
				}
				receivingRegenerate[d.ID] = e.Generated
			}
		}
		if receivingLabel[d.ID] == "" && d.ReceivingProvider != "" {
			receivingLabel[d.ID] = d.ReceivingProvider
		}
		receivingEditors[d.ID] = recvEditors
	}

	notice, secretLabel, secret := r.URL.Query().Get("notice"), "", ""
	workerCode, workerWebhook := "", ""
	// An external-alias connector flash is consumed only after the alias set is
	// known, so a stale flash is never applied to the wrong alias.
	var aliasFlash *externalAliasNoticeFlash
	if tok := r.URL.Query().Get("_flash"); tok != "" {
		if _, ok := s.flashes.peek(tok); ok {
			aliasFlash = s.takeExternalAliasFlash(tok, p, boxes)
		}
	}
	if tok := r.URL.Query().Get("_flash"); tok != "" && aliasFlash == nil {
		if v, ok := s.flashes.peek(tok); ok {
			switch f := v.(type) {
			case secretFlash:
				if taken, ok := s.flashes.take(tok); ok {
					if tf, ok := taken.(secretFlash); ok {
						notice, secretLabel, secret = tf.Notice, tf.Label, tf.Secret
					}
				}
			case domainWorkerFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if rc, err := s.Service.Store.GetDomainReceivingConfig(ctx, p.AccountID, f.DomainID); err == nil && rc.ID == f.ConfigID && rc.Revision == f.Revision {
						if taken, ok := s.flashes.take(tok); ok {
							if tf, ok := taken.(domainWorkerFlash); ok && tf.WorkerCode == f.WorkerCode && tf.ConfigID == f.ConfigID && tf.Revision == f.Revision && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == f.DomainID {
								workerCode, workerWebhook = tf.WorkerCode, tf.WebhookURL
							}
						}
					}
				}
			case domainNoticeFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if taken, ok := s.flashes.take(tok); ok {
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

	// External sending aliases: build each connector popup. The alias named in
	// the query (or a validation flash) opens its dialog. The capability is
	// Admin-only, so non-admin principals render no dialogs.
	var aliasDialogs []externalAliasDialogView
	if s.Service.ExternalAliasAdmin(p) == nil {
		openAlias := strings.TrimSpace(r.URL.Query().Get("alias"))
		if aliasFlash != nil {
			openAlias = aliasFlash.AliasID
		}
		aliasDialogs = s.externalAliasDialogs(ctx, p.AccountID, boxes, r.URL.Query().Get("provider"), openAlias, aliasFlash)
	}

	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, dashboardBody, pageData{Title: "Dashboard", Tab: "home", Principal: p, CSRF: csrf(r), Account: acc, BaseURL: s.Service.Config.BaseURL, Domains: domains, DomainSendingReady: sendingReady, DomainReceivingReady: receivingReady, DomainIsMX: domainIsMX, InboxSendingReady: inboxSendingReady, DomainInboxes: domainInboxes, DomainSendingEditors: sendingEditors, DomainReceivingEditors: receivingEditors, DomainSendingSelected: sendingSelected, DomainReceivingSelected: receivingSelected, DomainSendingLabel: sendingLabel, DomainReceivingLabel: receivingLabel, DomainReceivingRegenerate: receivingRegenerate, DialMXSetup: dialMXSetup, DomainParentCandidate: domainParentCandidate, DomainOpenID: openID, DomainOpenKind: openKind, DomainWorkerCode: workerCode, DomainWorkerWebhook: workerWebhook, DomainNamesCSV: domainNamesCSV(domains), Inboxes: boxes, Messages: msgs, Credentials: credentialViews(keys, nil, nil), InboxConnectors: inboxConnectors, Unread: unread, MailboxSizes: mailboxSizes, DraftCounts: draftCounts, InboxAddr: inboxAddrMap(boxes), ExternalAliasDialogs: aliasDialogs, InboxOpenID: inboxOpenID, InboxOpenTab: inboxOpenTab, InboxOpenConnectorID: inboxOpenConnectorID, Notice: notice, SecretLabel: secretLabel, Secret: secret})
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
	if _, err := s.Service.Store.CreateDomainWithOptions(r.Context(), p.AccountID, r.Form.Get("name"), opts); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/?notice=Domain+created", 303)
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
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	paths, err := s.Service.Store.PurgeDomain(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	http.Redirect(w, r, "/?notice=Domain+deleted", 303)
}
func (s *Server) uiCreateInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	cfg, err := parseInboxConfig(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	inbox, err := s.Service.Store.CreateInbox(r.Context(), p.AccountID, r.Form.Get("domain"), r.Form.Get("local"), r.Form.Get("display"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.applyInboxConfig(r.Context(), p.AccountID, inbox.ID, cfg); err != nil {
		http.Error(w, err.Error(), 400)
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

// aliasNamesForForms builds a name lookup keyed by lowercased address for a
// submitted alias set, so a names-only update can be merged onto the existing
// aliases.
func aliasNamesForForms(forms []aliasForm) map[string]string {
	out := map[string]string{}
	for _, f := range forms {
		if f.DisplayName != "" {
			out[strings.ToLower(f.LocalPart+"@"+f.DomainName)] = f.DisplayName
		}
	}
	return out
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
	return inboxConfig{
		allowedSenders:       senders,
		approverEmail:        approverEmail,
		senderRestricted:     r.Form.Get("sender_restricted") == "1",
		requireAuthenticated: r.Form.Get("require_authenticated") == "1",
		aliases:              aliases,
		defaultSender:        strings.ToLower(strings.TrimSpace(r.Form.Get("default_sender"))),
	}, nil
}

// applyInboxAliases resolves each submitted alias domain to an account-owned
// domain id and replaces the inbox's alias set.
func (s *Server) applyInboxAliases(ctx context.Context, accountID, inboxID string, aliases []aliasForm) error {
	inputs := make([]store.AliasInput, 0, len(aliases))
	if len(aliases) > 0 {
		domains, err := s.Service.Store.ListDomains(ctx, accountID)
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
	return s.Service.Store.SetInboxAliases(ctx, accountID, inboxID, inputs)
}

func (s *Server) applyInboxConfig(ctx context.Context, accountID, inboxID string, cfg inboxConfig) error {
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
	if err := s.applyInboxAliases(ctx, accountID, inboxID, cfg.aliases); err != nil {
		return err
	}
	// Applied after aliases so a just-set alias can be the default sender.
	return s.Service.Store.SetInboxDefaultSender(ctx, accountID, inboxID, cfg.defaultSender)
}

func (s *Server) uiUpdateInbox(w http.ResponseWriter, r *http.Request) {
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
	cfg, err := parseInboxConfig(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.Service.Store.SetInboxDisplayName(r.Context(), p.AccountID, id, r.Form.Get("display")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.applyInboxConfig(r.Context(), p.AccountID, id, cfg); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/?notice=Inbox+updated", 303)
}

func (s *Server) uiDeleteInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	paths, err := s.Service.Store.PurgeInbox(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	http.Redirect(w, r, "/?notice=Inbox+deleted", 303)
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
			http.Error(w, err.Error(), 400)
			return
		}
		if err := validateWebhookBearerSecret(authMode, r.Form.Get("bearer_secret")); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		plain, err := webhookSecret(r.Form.Get("bearer_secret"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		enc, err := s.Service.EncryptSecret([]byte(plain))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name := strings.TrimSpace(r.Form.Get("name"))
		if name == "" {
			name = "Webhook"
		}
		if _, err := s.Service.Store.CreateWebhookClient(r.Context(), p.AccountID, r.Form.Get("inbox"), name, strings.TrimSpace(r.Form.Get("url")), mode, authMode, enc); err != nil {
			http.Error(w, err.Error(), 400)
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
		gatewayID, gwSecret, deliveryKey, err := s.Service.CreateHermesRelay(r.Context(), p, inboxID, r.Form.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		notice, label, secret = "Hermes relay connection created", "Paste these lines into the gateway .env", hermesEnvBlock(s.Service.Config.BaseURL, gatewayID, gwSecret, deliveryKey)
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
		_, plain, err := s.Service.Store.CreateAPIKey(r.Context(), p.AccountID, r.Form.Get("name"), r.Form.Get("admin") == "1", roles)
		if err != nil {
			http.Error(w, err.Error(), 400)
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

// wantsJSON reports whether the caller asked for a JSON response, so the
// Add Client dialog can receive the one-time secret inline without a redirect.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// flashSecret stores a one-time secret and redirects to the dashboard, which
// consumes it (Post/Redirect/Get). A refresh then shows a plain dashboard.
func (s *Server) flashSecret(w http.ResponseWriter, r *http.Request, notice, label, secret string) {
	dest := "/"
	if tok := s.flashes.put(secretFlash{Notice: notice, Label: label, Secret: secret}, len(notice)+len(label)+len(secret)+64); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
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
	if err := s.Service.Store.UpdateAPIKey(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("name"), r.Form.Get("admin") == "1", roles); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
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
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
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
	if err := s.Service.Store.RevokeAPIKey(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
	http.Redirect(w, r, "/?notice=Key+deleted", 303)
}

func connectorSettingsRedirect(w http.ResponseWriter, r *http.Request, notice string, keepConnector bool) {
	dest := "/?notice=" + url.QueryEscape(notice)
	if inboxID := strings.TrimSpace(r.Form.Get("inbox")); inboxID != "" {
		dest += "&inbox=" + url.QueryEscape(inboxID) + "&inbox_tab=connectors"
		if keepConnector {
			if connectorID := strings.TrimSpace(r.PathValue("id")); connectorID != "" {
				dest += "&connector=" + url.QueryEscape(connectorID)
			}
		}
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
		http.Error(w, err.Error(), 400)
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" {
		name = "Webhook"
	}
	if err := validateWebhookBearerSecret(r.Form.Get("auth"), r.Form.Get("bearer_secret")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	encrypted := ""
	if secret := r.Form.Get("bearer_secret"); secret != "" {
		var err error
		encrypted, err = s.Service.EncryptSecret([]byte(secret))
		if err != nil {
			http.Error(w, "webhook client update failed", 500)
			return
		}
	}
	if err := s.Service.Store.UpdateWebhookClientWithSecret(r.Context(), p.AccountID, r.PathValue("id"), name, strings.TrimSpace(r.Form.Get("url")), r.Form.Get("mode"), r.Form.Get("auth"), encrypted); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	connectorSettingsRedirect(w, r, "Webhook updated", true)
}

func (s *Server) uiRotateWebhook(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	plain, err := auth.RandomToken(32)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	enc, err := s.Service.EncryptSecret([]byte(plain))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.Service.Store.RotateWebhookSecret(r.Context(), p.AccountID, r.PathValue("id"), enc); err != nil {
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
		return
	}
	connectorSettingsRedirect(w, r, "Webhook updated", true)
}

func (s *Server) uiDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteWebhookClient(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	connectorSettingsRedirect(w, r, "Webhook deleted", false)
}

func (s *Server) uiUpdateHermes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.UpdateHermesConnectionName(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("name")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if role := r.Form.Get("role"); role != "" {
		if err := s.Service.Store.SetHermesOutboundRole(r.Context(), p.AccountID, r.PathValue("id"), role); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	connectorSettingsRedirect(w, r, "Connection updated", true)
}

func (s *Server) uiDeleteHermes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteHermesConnection(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Hub.CancelScope("hrm:" + r.PathValue("id"))
	connectorSettingsRedirect(w, r, "Connection deleted", false)
}

// cloudflareWorkerCode renders the embedded Worker template with the given
// base URL and generated shared secret.
func (s *Server) cloudflareWorkerCode(baseURL, secret string) string {
	code := string(cloudflareWorkerTemplate)
	code = strings.ReplaceAll(code, "__MAILMOOSE_WEBHOOK_URL__", strings.TrimRight(baseURL, "/")+"/internal/ingest/cloudflare")
	code = strings.ReplaceAll(code, "__MAILMOOSE_WEBHOOK_SECRET__", secret)
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
		out = append(out, credentialView{ID: h.ID, Kind: kind, Name: h.Name, Type: typ, Scope: scope, InboxID: h.InboxID, Role: role, Enabled: true})
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

func hermesEnvBlock(baseURL, gatewayID, secret, deliveryKey string) string {
	return fmt.Sprintf("GATEWAY_RELAY_URL=%s\nGATEWAY_RELAY_ID=%s\nGATEWAY_RELAY_SECRET=%s\nGATEWAY_RELAY_DELIVERY_KEY=%s\nGATEWAY_RELAY_PLATFORMS=email\nGATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true",
		baseURL, gatewayID, secret, deliveryKey)
}

// createOpenClawConnector creates an OpenClaw relay connector (direct
// credentials) or mints a one-time setup code, depending on the setup method
// selected in the connector dialog. It returns the one-time notice, label and
// secret to show; on error it writes the response itself and returns empty
// strings.
func (s *Server) createOpenClawConnector(w http.ResponseWriter, r *http.Request, p model.Principal, inboxID string) (notice, label, secret string) {
	name := r.Form.Get("name")
	if r.Form.Get("setup") == "manual" {
		gatewayID, gwSecret, deliveryKey, err := s.Service.CreateRelay(r.Context(), p, inboxID, name, store.KindOpenClaw)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return "", "", ""
		}
		return "OpenClaw connector created",
			"Paste this config block into the OpenClaw host (~/.openclaw/openclaw.json), then restart the Gateway. The secret is shown only once.",
			openClawConfigBlock(s.Service.Config.BaseURL, gatewayID, gwSecret, deliveryKey)
	}
	code, err := s.Service.CreateRelayEnrollCode(r.Context(), p, inboxID, name, store.KindOpenClaw, 15*time.Minute)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return "", "", ""
	}
	base := strings.TrimRight(s.Service.Config.BaseURL, "/")
	return "OpenClaw setup code created",
		"Run this on the OpenClaw host (the code expires in 15 minutes):",
		"openclaw channels add --channel mailmoose --code " + base + "/#" + code
}

// openClawConfigBlock is the manual fallback for air-gapped installs: a
// channels.mailmoose JSON5 block carrying the minted credentials.
func openClawConfigBlock(baseURL, gatewayID, secret, deliveryKey string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return fmt.Sprintf(`{
  "channels": {
    "mailmoose": {
      "enabled": true,
      "baseUrl": %q,
      "gatewayId": %q,
      "secret": %q,
      "deliveryKey": %q
    }
  }
}`, baseURL, gatewayID, secret, deliveryKey)
}

const messageBody = `<div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{if not .OutboundReady}}<div class="banner warn">{{if .SendingPausedExternal}}Sending paused — configure the sending connector for the selected sender ({{.SendingPausedAddress}}). Mail will queue. <a href="{{.SendingPausedURL}}">Configure</a>.{{else}}Sending is paused until a provider is configured for this domain. Mail will queue. <a href="{{.DomainSendingSettingsURL}}">Add one</a>.{{end}}</div>{{end}}
{{if not .InboundReady}}<div class="banner warn">Not receiving — no receive path is configured for this domain. <a href="{{.DomainReceivingSettingsURL}}">Add one</a>.</div>{{end}}
<section class="card"><div class="msghead"><h1>{{if .Message.Subject}}{{.Message.Subject}}{{else}}(no subject){{end}}</h1><div class="actions">{{if .Message.DeletedAt}}<form method="post" action="/ui/messages/{{.Message.ID}}/restore"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary icon-btn" title="Restore" aria-label="Restore">` + iconRestore + `</button></form><form method="post" action="/ui/messages/{{.Message.ID}}/purge" data-confirm="Delete this message permanently? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary icon-btn danger" title="Delete forever" aria-label="Delete forever">` + iconDeleteFore + `</button></form>{{else}}<a class="btn secondary icon-btn" title="Reply" aria-label="Reply" href="/ui/messages/{{.Message.ID}}/reply">` + iconReply + `</a><a class="btn secondary icon-btn" title="Reply all" aria-label="Reply all" href="/ui/messages/{{.Message.ID}}/reply-all">` + iconReplyAll + `</a><a class="btn secondary icon-btn" title="Forward" aria-label="Forward" href="/ui/messages/{{.Message.ID}}/forward">` + iconForward + `</a><form method="post" action="/ui/messages/{{.Message.ID}}/read"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="read" value="0"><button class="secondary icon-btn" title="Mark unread" aria-label="Mark unread">` + iconMarkUnread + `</button></form><form method="post" action="/ui/messages/{{.Message.ID}}/delete" data-confirm="Move this message to Trash?"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary icon-btn danger" title="Move to trash" aria-label="Move to trash">` + iconTrash + `</button></form>{{end}}</div></div>
<div class="msgmeta"><p class="muted"><b>From:</b> {{if .Message.From.Name}}{{.Message.From.Name}} &lt;{{.Message.From.Address}}&gt;{{else}}{{.Message.From.Address}}{{end}}<br><b>To:</b> {{join .Message.To ", "}}{{if .Message.CC}}<br><b>Cc:</b> {{join .Message.CC ", "}}{{end}}<br><b>Date:</b> {{localDateTime .Message.CreatedAt}}{{if .Inbox}} · <b>Mailbox:</b> {{.Inbox.Address}}{{end}}</p><form class="labeladd" method="post" action="/ui/messages/{{.Message.ID}}/labels"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="action" value="add"><input name="label" placeholder="Add label" maxlength="64"><button class="btn-sm" title="Add label" aria-label="Add label">+</button></form></div>
{{if .Message.Labels}}<div class="labelbar"><b>Labels:</b>{{range .Message.Labels}}<form class="labelpill" method="post" action="/ui/messages/{{$.Message.ID}}/labels"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="remove"><input type="hidden" name="label" value="{{.}}"><span>{{.}}</span><button class="labelx" title="Remove label" aria-label="Remove label">×</button></form>{{end}}</div>{{end}}
{{if .Attachments}}<h3>Attachments</h3><ul class="attachments">{{range .Attachments}}<li><a href="/ui/attachments/{{.ID}}">{{.Filename}}</a> <span class="muted">· {{bytes .Size}}</span></li>{{end}}</ul>{{end}}
<hr>{{if .Message.HTML}}{{if .MessageHasRemoteImages}}<div class="banner warn" id="remote-img-banner" style="margin-bottom:8px">Remote images are hidden to prevent read-tracking. <button type="button" class="secondary btn-sm" data-remote-img-show>Show images</button></div>{{end}}<iframe class="mailframe" sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" loading="lazy" src="/ui/messages/{{.Message.ID}}/html" data-mailframe></iframe>{{else}}<div class="msgbody">{{linkify .Message.Text}}</div>{{end}}
{{if and .Message.HTML .Message.Text}}<details><summary>Plain text</summary><div class="msgbody">{{linkify .Message.Text}}</div></details>{{end}}</section>
{{if gt (len .ThreadMessages) 1}}<section class="card"><h3>Conversation ({{len .ThreadMessages}})</h3><table>{{range .ThreadMessages}}<tr><td class="muted">{{localDateTime .CreatedAt}}</td><td>{{if eq .Direction "outbound"}}To: {{join .To ", "}}{{else}}{{.From.Address}}{{end}}</td><td>{{if eq .ID $.Message.ID}}<b>{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</b>{{else}}<a href="/ui/messages/{{.ID}}">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</a>{{end}}</td></tr>{{end}}</table></section>{{end}}</div></div>`

func (s *Server) uiMessage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "message not found", 404)
		return
	}
	if !m.Read {
		read := true
		if err = s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read); err == nil {
			m.Read = true
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
		if a, ok := externalSenderFor(*box); ok {
			outboundReady = a.Configured
			if !a.Configured {
				pausedExternal, pausedAddress, pausedURL = true, a.Address, externalAliasConfigureURL(a.ID)
			}
		} else {
			_, outErr := s.Service.Store.ResolveDomainSendingConfig(r.Context(), p.AccountID, box.DomainID)
			outboundReady = outErr == nil
		}
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
		unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
		unreadCount = unread[box.ID]
		spamCount, _ = s.Service.Store.CountSpam(r.Context(), p, box.ID)
		trashCount, _ = s.Service.Store.CountTrash(r.Context(), p, box.ID)
		draftCount, _ = s.Service.Store.CountDrafts(r.Context(), p, box.ID)
		outboxCount, _ = s.Service.Store.CountOutbox(r.Context(), p, box.ID)
		inboxLabels, _ = s.Service.Store.ListInboxLabels(r.Context(), p, box.ID)
		labelUnread, _ = s.Service.Store.InboxLabelUnreadCounts(r.Context(), p, box.ID)
	}
	s.render(w, r, messageBody, pageData{Title: title, Principal: p, CSRF: csrf(r), Account: acc, Message: &m, MessageHasRemoteImages: htmlsanitize.HasRemoteImages(m.HTML), Attachments: atts, Inbox: box, Folder: messageFolder, Labels: inboxLabels, LabelUnread: labelUnread, UnreadCount: unreadCount, SpamCount: spamCount, TrashCount: trashCount, DraftCount: draftCount, OutboxCount: outboxCount, ThreadMessages: thread, Notice: r.URL.Query().Get("notice"), OutboundReady: outboundReady, InboundReady: inboundReady, SendingPausedExternal: pausedExternal, SendingPausedAddress: pausedAddress, SendingPausedURL: pausedURL, DomainSendingSettingsURL: domainSendingURL, DomainReceivingSettingsURL: domainReceivingURL})
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
// of a long webhook URL.
func shortTooltipURL(raw string) string {
	raw = strings.TrimSpace(raw)
	const max = 56
	if len(raw) <= max {
		return raw
	}
	const tail = 14
	return raw[:max-tail-4] + "...." + raw[len(raw)-tail:]
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

// formatLocalDateTime renders a stored (UTC) timestamp in loc as a full date.
func formatLocalDateTime(loc *time.Location, t time.Time) string {
	return t.In(loc).Format("2006-01-02 15:04")
}

func filesize(n int64) string {
	const unit = 1024 * 1024
	return fmt.Sprintf("%.1f MB", float64(n)/float64(unit))
}
