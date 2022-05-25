package commands

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"

	pat "github.com/cloudflare/pat-go"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	challengeNonceLength = 32
)

var (
	// WWW-Authenticate authorization challenge attributes
	authorizationAttributeChallenge = "challenge"
	authorizationAttributeMaxAge    = "max-age"
	authorizationAttributeTokenKey  = "token-key"
	authorizationAttributeNameKey   = "issuer-encap-key"

	// Headers clients can send to control the types of token challenges sent
	headerTokenAttributeNoninteractive = "Sec-Token-Attribute-Non-Interactive"
	headerTokenAttributeCrossOrigin    = "Sec-Token-Attribute-Cross-Origin"
	headerTokenAttributeChallengeCount = "Sec-Token-Attribute-Count"
	headerTokenType                    = "Sec-CH-Token-Type"

	// Type of authorization
	privateTokenType = "PrivateToken"

	// Test resource to load upon token success
	testResource = "https://tfpauly.github.io/privacy-proxy/draft-privacypass-rate-limit-tokens.html"
)

type Origin struct {
	issuerName             string
	originName             string
	additionalOriginInfo   []string
	rateLimitedTokenKeyEnc []byte // Encoding of validation public key
	rateLimitedTokenKey    *rsa.PublicKey
	basicTokenKeyEnc       []byte // Encoding of validation public key
	basicValidationKey     *rsa.PublicKey
	issuerEncapKey         pat.EncapKey

	// Map from challenge hash to list of outstanding challenges
	challenges    map[string][]pat.TokenChallenge
	challengeLock sync.Mutex
}

func (o Origin) CreateChallenge(req *http.Request) (string, string) {
	nonce := make([]byte, challengeNonceLength)
	rand.Reader.Read(nonce)
	originInfo := []string{o.originName}
	for _, originName := range o.additionalOriginInfo {
		originInfo = append(originInfo, originName)
	}

	if req.Header.Get(headerTokenAttributeNoninteractive) != "" || req.URL.Query().Get("noninteractive") != "" {
		// If the client requested a non-interactive token, then clear out the nonce slot
		nonce = []byte{} // empty slice
	}
	if req.Header.Get(headerTokenAttributeCrossOrigin) != "" || req.URL.Query().Get("crossorigin") != "" {
		// If the client requested a cross-origin token, then clear out the origin slot
		originInfo = nil
	}

	tokenKey := base64.URLEncoding.EncodeToString(o.rateLimitedTokenKeyEnc)
	tokenType := pat.RateLimitedTokenType // default
	if req.Header.Get(headerTokenType) != "" || req.URL.Query().Get("type") != "" {
		tokenTypeValue, err := strconv.Atoi(req.Header.Get(headerTokenType))
		if err == nil {
			if tokenTypeValue == int(pat.BasicPublicTokenType) {
				tokenType = pat.BasicPublicTokenType
				tokenKey = base64.URLEncoding.EncodeToString(o.basicTokenKeyEnc)
			}
		} else {
			tokenTypeValue, err = strconv.Atoi(req.URL.Query().Get("type"))
			if err == nil {
				if tokenTypeValue == int(pat.BasicPublicTokenType) {
					tokenType = pat.BasicPublicTokenType
					tokenKey = base64.URLEncoding.EncodeToString(o.basicTokenKeyEnc)
				}
			}
		}
	}

	challenge := pat.TokenChallenge{
		TokenType:       tokenType,
		IssuerName:      o.issuerName,
		OriginInfo:      originInfo,
		RedemptionNonce: nonce,
	}

	// Add to the running list of challenges
	challengeEnc := challenge.Marshal()
	context := sha256.Sum256(challengeEnc)
	contextEnc := hex.EncodeToString(context[:])

	// Acquire the lock and write
	o.challengeLock.Lock()
	defer o.challengeLock.Unlock()
	_, ok := o.challenges[contextEnc]
	if !ok {
		o.challenges[contextEnc] = make([]pat.TokenChallenge, 0)
	}
	o.challenges[contextEnc] = append(o.challenges[contextEnc], challenge)
	log.Debugln("Adding challenge context", contextEnc)

	return base64.URLEncoding.EncodeToString(challengeEnc), tokenKey
}

func (o Origin) handleRequest(w http.ResponseWriter, req *http.Request) {
	reqEnc, _ := httputil.DumpRequest(req, false)
	log.Debugln("Handling request:", string(reqEnc))

	// If the Authorization header is empty, challenge the client for a token
	if req.Header.Get("Authorization") == "" {
		log.Debugln("Missing authorization header. Replying with challenge.")

		count := 1
		if countReq := req.Header.Get(headerTokenAttributeChallengeCount); countReq != "" {
			countVal, err := strconv.Atoi(countReq)
			if err == nil && countVal > 0 && countVal < 10 {
				// These bounds are arbitrary
				count = countVal
			}
		}
		challengeList := ""
		for i := 0; i < count; i++ {
			challengeEnc, tokenKeyEnc := o.CreateChallenge(req)
			challengeString := authorizationAttributeChallenge + "=" + challengeEnc
			issuerKeyString := authorizationAttributeTokenKey + "=" + tokenKeyEnc
			maxAgeString := authorizationAttributeMaxAge + "=" + "10"
			issuerEncapKeyString := authorizationAttributeNameKey + "=" + base64.URLEncoding.EncodeToString(o.issuerEncapKey.Marshal()) // This might be ignored by clients
			challengeList = challengeList + privateTokenType + " " + challengeString + ", " + issuerKeyString + "," + issuerEncapKeyString + ", " + maxAgeString
		}

		w.Header().Set("WWW-Authenticate", challengeList)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	authValue := req.Header.Get("Authorization")
	tokenPrefix := privateTokenType + " " + "token="
	if !strings.HasPrefix(authValue, tokenPrefix) {
		log.Debugln("Authorization header missing 'PrivateToken token=' prefix")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	tokenValueEnc := strings.SplitAfter(authValue, tokenPrefix)[1] // XXX(caw): there's probably a better way to parse this out
	tokenValue, err := base64.URLEncoding.DecodeString(tokenValueEnc)
	if err != nil {
		log.Debugln("Failed reading Authorization header token value")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	token, err := pat.UnmarshalToken(tokenValue)
	if err != nil {
		log.Debugln("Failed decoding Token")
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	tokenContextEnc := hex.EncodeToString(token.Context)
	challengeList, ok := o.challenges[tokenContextEnc]
	if !ok {
		log.Debugln("No outstanding challenge matching context", tokenContextEnc)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	// Consume the first matching challenge
	challenge := challengeList[0]
	o.challenges[tokenContextEnc] = o.challenges[tokenContextEnc][1:]
	log.Debugln("Consuming challenge context", tokenContextEnc)
	log.Debugln("Remainder matching challenge set size", len(o.challenges[tokenContextEnc]))
	if len(o.challenges[tokenContextEnc]) == 0 {
		delete(o.challenges, tokenContextEnc)
	}

	authInput := token.AuthenticatorInput()
	key := o.rateLimitedTokenKey
	if challenge.TokenType == pat.BasicPublicTokenType {
		key = o.basicValidationKey
	}

	hash := sha512.New384()
	hash.Write(authInput)
	digest := hash.Sum(nil)
	err = rsa.VerifyPSS(key, crypto.SHA384, digest, token.Authenticator, &rsa.PSSOptions{
		Hash:       crypto.SHA384,
		SaltLength: crypto.SHA384.Size(),
	})
	if err != nil {
		// Token validation failed
		log.Debugln("Token validation failed", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// Fetch the test resource for the client
	httpClient := &http.Client{}
	resourceReq, err := http.NewRequest(http.MethodGet, testResource, nil)
	if err != nil {
		log.Debugln(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := httpClient.Do(resourceReq)
	if err != nil {
		log.Debugln(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Debugln(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(body)
}

func startOrigin(c *cli.Context) error {
	cert := c.String("cert")
	key := c.String("key")
	port := c.String("port")
	issuer := c.String("issuer")
	name := c.String("name")
	originInfo := c.StringSlice("origin-info")
	logLevel := c.String("log")

	if cert == "" {
		log.Fatal("Invalid key material (missing certificate). See README for configuration.")
	}
	if key == "" {
		log.Fatal("Invalid key material (missing private key). See README for configuration.")
	}
	if issuer == "" {
		log.Fatal("Invalid issuer. See README for configuration.")
	}
	if name == "" {
		log.Fatal("Invalid origin name. See README for configuration.")
	}

	switch logLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	}

	issuerConfig, err := fetchIssuerConfig(issuer)
	if err != nil {
		return err
	}

	var basicValidationKeyEnc []byte
	var basicValidationKey *rsa.PublicKey
	var rateLimitedTokenKeyEnc []byte
	var rateLimitedTokenKey *rsa.PublicKey
	for i := 0; i < len(issuerConfig.TokenKeys); i++ {
		switch issuerConfig.TokenKeys[i].TokenType {
		case int(pat.BasicPublicTokenType):
			basicValidationKeyEnc, err = base64.URLEncoding.DecodeString(issuerConfig.TokenKeys[i].TokenKey)
			if err != nil {
				log.Fatal(err)
			}
			basicValidationKey, err = pat.UnmarshalTokenKey(basicValidationKeyEnc)
			if err != nil {
				log.Fatal(err)
			}
		case int(pat.RateLimitedTokenType):
			rateLimitedTokenKeyEnc, err = base64.URLEncoding.DecodeString(issuerConfig.TokenKeys[i].TokenKey)
			if err != nil {
				log.Fatal(err)
			}
			rateLimitedTokenKey, err = pat.UnmarshalTokenKey(rateLimitedTokenKeyEnc)
			if err != nil {
				log.Fatal(err)
			}
		}
	}

	nameKeyURI, err := composeURL(issuer, issuerConfig.IssuerEncapKeyURI)
	if err != nil {
		return err
	}
	originNameKey, err := fetchIssuerNameKey(nameKeyURI)
	if err != nil {
		return err
	}

	origin := Origin{
		issuerName:             issuer,
		originName:             name,
		additionalOriginInfo:   originInfo,
		issuerEncapKey:         originNameKey,
		rateLimitedTokenKeyEnc: rateLimitedTokenKeyEnc,
		rateLimitedTokenKey:    rateLimitedTokenKey,
		basicTokenKeyEnc:       basicValidationKeyEnc,
		basicValidationKey:     basicValidationKey,
		challenges:             make(map[string][]pat.TokenChallenge),
		challengeLock:          sync.Mutex{},
	}

	http.HandleFunc("/", origin.handleRequest)
	err = http.ListenAndServeTLS(":"+port, cert, key, nil)
	if err != nil {
		log.Fatal("ListenAndServeTLS: ", err)
	}
	return err
}
