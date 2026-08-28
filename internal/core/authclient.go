package core

import (
	"context"
	"fmt"

	"google.golang.org/grpc/metadata"

	authpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/auth"
)

// AuthOrg is one org-membership row in the CLI's neutral shape (decoupled from
// the pb type so cmd packages don't all import authpb).
type AuthOrg struct {
	ID   string
	Slug string
	Name string
	Kind string // private | company
	Role string // owner | admin | member
}

// SignIn authenticates email+password against the console auth gateway at addr
// and returns the minted bearer + the principal's user id. The RPC is
// unauthenticated (it is the credential that mints the bearer). A wrong
// email/password surfaces as a gRPC Unauthenticated error (anti-enumeration —
// the server maps every failure to one opaque status).
func SignIn(ctx context.Context, addr, email, password string) (token, userID string, err error) {
	cl, conn, err := DialAuthService(addr)
	if err != nil {
		return "", "", fmt.Errorf("dial console: %w", err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := cl.SignIn(ctx, &authpb.SignInReq{Email: email, Password: password})
	if err != nil {
		return "", "", err
	}
	if resp.GetToken() == "" {
		return "", "", fmt.Errorf("invalid email or password")
	}
	return resp.GetToken(), resp.GetUserId(), nil
}

// ListMyOrgs lists the organizations the bearer belongs to, dialing the console
// auth gateway at addr and threading token as the request bearer. A nil/empty
// result is not an error (a fresh account may belong to no org yet).
func ListMyOrgs(ctx context.Context, addr, token string) ([]AuthOrg, error) {
	cl, conn, err := DialAuthService(addr)
	if err != nil {
		return nil, fmt.Errorf("dial console: %w", err)
	}
	defer func() { _ = conn.Close() }()
	authCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	resp, err := cl.ListMyOrgs(authCtx, &authpb.ListMyOrgsReq{})
	if err != nil {
		return nil, err
	}
	orgs := resp.GetOrgs()
	out := make([]AuthOrg, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, AuthOrg{
			ID:   o.GetOrgId(),
			Slug: o.GetSlug(),
			Name: o.GetName(),
			Kind: o.GetKind(),
			Role: o.GetRole(),
		})
	}
	return out, nil
}
