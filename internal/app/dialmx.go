package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func (s *Service) DialMXBackend() mxdial.Backend { return dialMXBackend{service: s} }

func (s *Service) PrivateMXBackend() mxdial.Backend {
	return dialMXBackend{service: s, private: true}
}

type dialMXBackend struct {
	service *Service
	private bool
}

func (b dialMXBackend) Domains(ctx context.Context) ([]mxdial.Domain, error) {
	if b.private {
		return nil, nil
	}
	domains, err := b.service.Store.ListDialMXDomains(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mxdial.Domain, 0, len(domains))
	for _, d := range domains {
		credential, err := b.service.EnsureDialMXCredential(ctx, d.AccountID, d.ID)
		if err != nil {
			return nil, err
		}
		cfg, err := b.service.Store.ResolveDomainReceivingConfig(ctx, d.AccountID, d.ID, "dialmx")
		if err != nil {
			continue
		}
		values, err := b.service.DecryptDomainReceivingConfig(cfg)
		if err != nil {
			return nil, err
		}
		seed, err := b.service.DecryptSecretAAD(dialMXSeedAAD(d.AccountID, d.ID), credential.EncryptedPrivateSeed)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("invalid Dial MX credential")
		}
		urls, _ := values["receiver_urls"].(string)
		contactEmail, _ := values["contact_email"].(string)
		setupID, _ := values["setup_id"].(string)
		out = append(out, mxdial.Domain{
			AccountID:    d.AccountID,
			ID:           d.ID,
			Name:         d.Name,
			KeyID:        credential.KeyID,
			PrivateKey:   ed25519.NewKeyFromSeed(seed),
			ReceiverURLs: splitReceiverURLs(urls),
			ContactEmail: contactEmail,
			SetupID:      setupID,
		})
	}
	return out, nil
}

func (b dialMXBackend) Resolve(ctx context.Context, domain string, recipients []string) (mxwire.ResolveResponse, error) {
	response := mxwire.ResolveResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeOK}
	if len(recipients) == 0 {
		return mxwire.ResolveResponse{MachineCode: mxwire.CodeUnknownRecipient}, nil
	}
	if !strings.EqualFold(domain, domainOf(recipients[0])) {
		return mxwire.ResolveResponse{MachineCode: mxwire.CodeUnauthorized}, nil
	}
	for _, recipient := range recipients {
		if !strings.EqualFold(domain, domainOf(recipient)) {
			return mxwire.ResolveResponse{MachineCode: mxwire.CodeUnauthorized}, nil
		}
	}
	provider := "dialmx"
	if b.private {
		provider = mxProvider
	}
	results := b.service.resolveMXRecipients(ctx, provider, recipients)
	for _, result := range results {
		response.Results = append(response.Results, mxwire.ResolveRecipient{Recipient: result.Recipient, Accept: result.Accept, Domain: result.Domain, Code: string(result.Code), Temporary: result.Temporary})
	}
	return response, nil
}

func (b dialMXBackend) Ingest(ctx context.Context, domains []string, meta mxwire.IngestMetadata, rawPath string) (mxwire.IngestResponse, error) {
	if len(domains) == 0 || len(meta.Recipients) == 0 {
		return mxwire.IngestResponse{MachineCode: mxwire.CodeUnauthorized}, nil
	}
	for _, recipient := range meta.Recipients {
		if !containsFold(domains, domainOf(recipient)) {
			return mxwire.IngestResponse{MachineCode: mxwire.CodeUnauthorized}, nil
		}
	}
	input := MXIngestInput{Recipients: meta.Recipients, EnvelopeFrom: meta.EnvelopeFrom, RawPath: rawPath, Size: meta.Size, ContentDigest: meta.ContentDigest, AuthResults: meta.AuthResults, TrustedAuth: true, ProviderMessageID: meta.ProviderMessageID}
	var result MXIngestResult
	var err error
	if b.private {
		result, err = b.service.IngestMX(ctx, input)
	} else {
		result, err = b.service.IngestDialMX(ctx, input)
	}
	if err != nil {
		return mxwire.IngestResponse{}, err
	}
	response := mxwire.IngestResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeOK, MessageID: result.MessageID, PerRecipient: result.PerRecipient}
	for _, r := range result.PerRecipient {
		if r.MachineCode != mxwire.CodeOK && r.MachineCode != mxwire.CodeDuplicate {
			response.MachineCode = r.MachineCode
		}
	}
	return response, nil
}

func splitReceiverURLs(raw string) []string {
	var out []string
	for _, receiver := range strings.Split(raw, ",") {
		if receiver = strings.TrimSpace(receiver); receiver != "" {
			out = append(out, receiver)
		}
	}
	return out
}

func containsFold(values []string, value string) bool {
	for _, item := range values {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return true
		}
	}
	return false
}
