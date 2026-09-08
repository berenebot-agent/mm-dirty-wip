package store

import (
	"context"
	"database/sql"
	"time"

	"gatehouse-mail/internal/auth"
	"gatehouse-mail/internal/idgen"
)

type HermesConnection struct { ID,AccountID,InboxID,Name,GatewayID,SecretEncrypted,DeliveryKeyEncrypted string; LastAckEventID int64; CreatedAt time.Time; LastConnectedAt *time.Time }

func (s *Store) CreateHermesEnrollToken(ctx context.Context,accountID,inboxID,name string,ttl time.Duration)(string,error){var n int;if err:=s.read.QueryRowContext(ctx,`SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`,inboxID,accountID).Scan(&n);err!=nil||n!=1{return "",ErrForbidden};tok,err:=auth.RandomToken(32);if err!=nil{return "",err};id:=idgen.New("hen");_,err=s.write.ExecContext(ctx,`INSERT INTO hermes_enroll_tokens(id,account_id,inbox_id,name,token_hash,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`,id,accountID,inboxID,name,auth.HashToken(tok),timeText(time.Now().UTC().Add(ttl)),nowText());return tok,err}

type EnrollRecord struct { AccountID,InboxID,Name string }
func (s *Store) ConsumeHermesEnrollToken(ctx context.Context,token string)(EnrollRecord,error){tx,err:=s.write.BeginTx(ctx,nil);if err!=nil{return EnrollRecord{},err};defer tx.Rollback();var r EnrollRecord;var id,exp string;var used sql.NullString;err=tx.QueryRowContext(ctx,`SELECT id,account_id,inbox_id,name,expires_at,used_at FROM hermes_enroll_tokens WHERE token_hash=?`,auth.HashToken(token)).Scan(&id,&r.AccountID,&r.InboxID,&r.Name,&exp,&used);if err==sql.ErrNoRows{return r,ErrNotFound};if err!=nil{return r,err};if used.Valid||parseTime(exp).Before(time.Now().UTC()){return r,ErrForbidden};if _,err=tx.ExecContext(ctx,`UPDATE hermes_enroll_tokens SET used_at=? WHERE id=?`,nowText(),id);err!=nil{return r,err};if err=tx.Commit();err!=nil{return r,err};return r,nil}
func (s *Store) CreateHermesConnection(ctx context.Context,r EnrollRecord,gatewayID,secretEnc,deliveryEnc string)(HermesConnection,error){id:=idgen.New("hrm");now:=nowText();_,err:=s.write.ExecContext(ctx,`INSERT INTO hermes_connections(id,account_id,inbox_id,name,gateway_id,secret_encrypted,delivery_key_encrypted,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(gateway_id) DO UPDATE SET account_id=excluded.account_id,inbox_id=excluded.inbox_id,name=excluded.name,secret_encrypted=excluded.secret_encrypted,delivery_key_encrypted=excluded.delivery_key_encrypted,last_ack_event_id=0,created_at=excluded.created_at`,id,r.AccountID,r.InboxID,r.Name,gatewayID,secretEnc,deliveryEnc,now);if err!=nil{return HermesConnection{},err};return s.GetHermesConnectionByGateway(ctx,gatewayID)}
func scanHermes(row interface{Scan(...any)error})(HermesConnection,error){var h HermesConnection;var cr string;var lc sql.NullString;err:=row.Scan(&h.ID,&h.AccountID,&h.InboxID,&h.Name,&h.GatewayID,&h.SecretEncrypted,&h.DeliveryKeyEncrypted,&h.LastAckEventID,&cr,&lc);if err!=nil{return h,err};h.CreatedAt=parseTime(cr);h.LastConnectedAt=nullableTime(lc);return h,nil}
func (s *Store) GetHermesConnectionByGateway(ctx context.Context,gatewayID string)(HermesConnection,error){h,err:=scanHermes(s.read.QueryRowContext(ctx,`SELECT id,account_id,inbox_id,name,gateway_id,secret_encrypted,delivery_key_encrypted,last_ack_event_id,created_at,last_connected_at FROM hermes_connections WHERE gateway_id=?`,gatewayID));if err==sql.ErrNoRows{return h,ErrNotFound};return h,err}
func (s *Store) ListHermesConnections(ctx context.Context,accountID string)([]HermesConnection,error){rows,err:=s.read.QueryContext(ctx,`SELECT id,account_id,inbox_id,name,gateway_id,secret_encrypted,delivery_key_encrypted,last_ack_event_id,created_at,last_connected_at FROM hermes_connections WHERE account_id=? ORDER BY created_at DESC`,accountID);if err!=nil{return nil,err};defer rows.Close();var out []HermesConnection;for rows.Next(){h,err:=scanHermes(rows);if err!=nil{return nil,err};out=append(out,h)};return out,rows.Err()}
func (s *Store) MarkHermesConnected(ctx context.Context,id string){_,_=s.write.ExecContext(ctx,`UPDATE hermes_connections SET last_connected_at=? WHERE id=?`,nowText(),id)}
func (s *Store) AckHermesEvent(ctx context.Context,id string,eventID int64)error{_,err:=s.write.ExecContext(ctx,`UPDATE hermes_connections SET last_ack_event_id=MAX(last_ack_event_id,?) WHERE id=?`,eventID,id);return err}
func (s *Store) DeleteHermesConnection(ctx context.Context,accountID,id string)error{res,err:=s.write.ExecContext(ctx,`DELETE FROM hermes_connections WHERE id=? AND account_id=?`,id,accountID);if err!=nil{return err};n,_:=res.RowsAffected();if n==0{return ErrNotFound};return nil}
