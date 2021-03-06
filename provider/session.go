package provider

import (
	"errors"
	"math/big"
	"net/url"
	"strconv"
	"time"

	"github.com/cknave/GOpenID"
	"github.com/cknave/GOpenID/dh"
)

var (
	ErrKnownNonce = errors.New("nonce is known")
)

type Session interface {
	SetProvider(*Provider)
	SetRequest(Request)
	GetRequest() Request
	GetResponse() (Response, error)
}

func SessionFromMessage(p *Provider, method string, msg gopenid.Message) (s Session, err error) {
	req, err := RequestFromMessage(method, msg)
	if err != nil {
		return
	}

	switch req.(type) {
	case *checkIDRequest:
		s = new(CheckIDSession)
	case *associateRequest:
		s = new(AssociateSession)
	case *checkAuthenticationRequest:
		s = new(CheckAuthenticationSession)
	}

	s.SetRequest(req)
	s.SetProvider(p)
	return
}

type CheckIDSession struct {
	provider *Provider
	request  *checkIDRequest

	accepted  bool
	identity  string
	claimedId string
}

func (s *CheckIDSession) SetProvider(p *Provider) {
	s.provider = p
}

func (s *CheckIDSession) SetRequest(r Request) {
	s.request = r.(*checkIDRequest)
}

func (s *CheckIDSession) GetRequest() Request {
	return s.request
}

func (s *CheckIDSession) Accept(identity, claimedId string) {
	s.accepted = true
	s.identity = identity
	s.claimedId = claimedId
}

func (s *CheckIDSession) GetResponse() (Response, error) {
	return s.buildResponse()
}

func (s *CheckIDSession) buildResponse() (res *openIDResponse, err error) {
	if s.accepted {
		res, err = s.getAcceptedResponse()
		if err != nil {
			return
		}

		order := []string{
			"op_endpoint",
			"return_to",
			"response_nonce",
			"assoc_handle",
			"claimed_id",
			"identity",
		}

		if _, ok := res.message.GetArg(gopenid.NewMessageKey(res.message.GetOpenIDNamespace(), "identity")); !ok {
			order = order[:5]
		}

		if _, ok := res.message.GetArg(gopenid.NewMessageKey(res.message.GetOpenIDNamespace(), "claimed_id")); !ok {
			copy(order[4:], order[len(order)-1:])
			order = order[:len(order)-1]
		}

		err = s.provider.signer.Sign(res, s.request.assocHandle.String(), order)
	} else {
		res = s.getRejectedResponse()
	}

	return
}

func (s *CheckIDSession) getAcceptedResponse() (res *openIDResponse, err error) {
	var (
		identity  gopenid.MessageValue
		claimedId gopenid.MessageValue
	)

	switch s.request.identity.String() {
	case gopenid.NsIdentifierSelect.String():
		if s.identity == "" {
			err = ErrIdentityNotSet
			return
		}

		identity = gopenid.MessageValue(s.identity)
		claimedId = gopenid.MessageValue(s.claimedId)
		if claimedId == "" {
			claimedId = identity
		}
	case s.identity:
		identity = s.request.identity
		claimedId = s.request.claimedId
	case "":
		if s.identity != "" {
			err = ErrIdentitySet
			return
		}
	default:
		err = ErrIdentityNotMatched
		return
	}

	res = newOpenIDResponse(s.request)
	res.AddArg(gopenid.NewMessageKey(s.request.GetNamespace(), "mode"), "id_res")
	res.AddArg(
		gopenid.NewMessageKey(s.request.GetNamespace(), "op_endpoint"),
		gopenid.MessageValue(s.provider.endpoint),
	)
	res.AddArg(gopenid.NewMessageKey(s.request.GetNamespace(), "claimed_id"), claimedId)
	res.AddArg(gopenid.NewMessageKey(s.request.GetNamespace(), "identity"), identity)
	res.AddArg(gopenid.NewMessageKey(s.request.GetNamespace(), "return_to"), s.request.returnTo)

	nonce := gopenid.GenerateNonce(time.Now().UTC())
	s.provider.store.StoreNonce(nonce.String())
	res.AddArg(
		gopenid.NewMessageKey(s.request.GetNamespace(), "response_nonce"),
		nonce,
	)
	return
}

func (s *CheckIDSession) getRejectedResponse() (res *openIDResponse) {
	res = newOpenIDResponse(s.request)

	var mode gopenid.MessageValue = "cancel"
	if s.request.mode == "checkid_immediate" {
		mode = "setup_needed"

		setupmsg := s.request.message.Copy()
		setupmsg.AddArg(
			gopenid.NewMessageKey(s.request.GetNamespace(), "mode"),
			"checkid_setup",
		)
		setupUrl, _ := url.Parse(s.provider.endpoint)
		setupUrl.RawQuery = setupmsg.ToQuery().Encode()
		res.AddArg(
			gopenid.NewMessageKey(s.request.GetNamespace(), "user_setup_url"),
			gopenid.MessageValue(setupUrl.String()),
		)
	}
	res.AddArg(gopenid.NewMessageKey(s.request.GetNamespace(), "mode"), mode)

	return
}

type AssociateSession struct {
	provider *Provider
	request  *associateRequest
}

func (s *AssociateSession) SetProvider(p *Provider) {
	s.provider = p
}

func (s *AssociateSession) SetRequest(r Request) {
	s.request = r.(*associateRequest)
}

func (s *AssociateSession) GetRequest() Request {
	return s.request
}

func (s *AssociateSession) GetResponse() (Response, error) {
	return s.buildResponse()
}

func (s *AssociateSession) buildResponse() (res *openIDResponse, err error) {
	if s.request.err != nil {
		return s.buildFailedResponse(s.request.err.Error()), nil
	}

	assoc, err := s.provider.signer.createAssociation(s.request.assocType, false)
	if err != nil {
		return s.buildFailedResponse(err.Error()), nil
	}

	s.provider.store.StoreAssociation(assoc)

	res = newOpenIDResponse(s.request)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "assoc_handle"),
		gopenid.MessageValue(assoc.GetHandle()),
	)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "session_type"),
		gopenid.MessageValue(s.request.sessionType.Name()),
	)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "assoc_type"),
		gopenid.MessageValue(s.request.assocType.Name()),
	)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "expires_in"),
		gopenid.MessageValue(strconv.FormatInt(assoc.GetExpires().Unix(), 10)),
	)

	if s.request.sessionType.Name() == gopenid.SessionNoEncryption.Name() {
		macKey := gopenid.EncodeBase64(assoc.GetSecret())

		res.AddArg(
			gopenid.NewMessageKey(res.GetNamespace(), "mac_key"),
			gopenid.MessageValue(macKey),
		)
	} else {
		var (
			X   = new(big.Int).SetBytes(assoc.GetSecret())
			Y   = new(big.Int).Exp(s.request.dhParams.G, X, s.request.dhParams.P)
			key = &dh.PrivateKey{
				X:      X,
				Params: s.request.dhParams,
				PublicKey: dh.PublicKey{
					Y: Y,
				},
			}
		)

		serverPublic := gopenid.EncodeBase64(key.PublicKey.Y.Bytes())
		res.AddArg(
			gopenid.NewMessageKey(res.GetNamespace(), "dh_server_public"),
			gopenid.MessageValue(serverPublic),
		)

		secret := assoc.GetSecret()

		shared := key.SharedSecret(s.request.dhConsumerPublic)
		h := s.request.assocType.Hash()
		h.Write(shared.ZZ.Bytes())
		hashedShared := h.Sum(nil)

		encMacKey := make([]byte, s.request.assocType.GetSecretSize())
		for i := 0; i < s.request.assocType.GetSecretSize(); i++ {
			encMacKey[i] = hashedShared[i] ^ secret[i]
		}
		encMacKey = gopenid.EncodeBase64(encMacKey)
		res.AddArg(
			gopenid.NewMessageKey(res.GetNamespace(), "enc_mac_key"),
			gopenid.MessageValue(encMacKey),
		)
	}

	return
}

func (s *AssociateSession) buildFailedResponse(err string) (res *openIDResponse) {
	res = newOpenIDResponse(s.request)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "error"),
		gopenid.MessageValue(err),
	)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "error_code"),
		"unsupported-type",
	)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "session_type"),
		gopenid.MessageValue(gopenid.DefaultSession.Name()),
	)
	res.AddArg(
		gopenid.NewMessageKey(res.GetNamespace(), "assoc_type"),
		gopenid.MessageValue(gopenid.DefaultAssoc.Name()),
	)

	return
}

type CheckAuthenticationSession struct {
	provider *Provider
	request  *checkAuthenticationRequest
}

func (s *CheckAuthenticationSession) SetProvider(p *Provider) {
	s.provider = p
}

func (s *CheckAuthenticationSession) SetRequest(r Request) {
	s.request = r.(*checkAuthenticationRequest)
}

func (s *CheckAuthenticationSession) GetRequest() Request {
	return s.request
}

func (s *CheckAuthenticationSession) GetResponse() (Response, error) {
	return s.buildResponse()
}

func (s *CheckAuthenticationSession) buildResponse() (res *openIDResponse, err error) {
	if s.provider.store.IsKnownNonce(s.request.responseNonce.String()) {
		err = ErrKnownNonce
		return
	}

	isValid, err := s.provider.signer.Verify(s.request, true)
	if err != nil {
		return
	}

	res = newOpenIDResponse(s.request)

	if isValid {
		res.AddArg(gopenid.NewMessageKey(res.GetNamespace(), "is_valid"), "true")
	} else {
		res.AddArg(gopenid.NewMessageKey(res.GetNamespace(), "is_valid"), "false")

		invalidateHandle, _ := s.request.message.GetArg(gopenid.NewMessageKey(s.request.GetNamespace(), "assoc_handle"))
		res.AddArg(
			gopenid.NewMessageKey(res.GetNamespace(), "invalidate_handle"),
			invalidateHandle,
		)
	}

	s.provider.signer.Invalidate(s.request.assocHandle.String(), true)
	return
}
