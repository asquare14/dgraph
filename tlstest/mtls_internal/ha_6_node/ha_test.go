package ha_6_node

import (
	"context"
	"github.com/dgraph-io/dgo/v200"
	"github.com/dgraph-io/dgo/v200/protos/api"
	"github.com/dgraph-io/dgraph/testutil"
	"github.com/dgraph-io/dgraph/x"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"testing"
	"time"
)

func runTests(t *testing.T, client *dgo.Dgraph) {
	type testCase struct {
		query      string
		wantResult string
	}
	suite := func(initialSchema string, setJSON string, cases []testCase) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		require.NoError(t, client.Alter(ctx, &api.Operation{
			DropAll: true,
		}))
		require.NoError(t, client.Alter(ctx, &api.Operation{
			Schema: initialSchema,
		}))

		txn := client.NewTxn()
		_, err := txn.Mutate(ctx, &api.Mutation{SetJson: []byte(setJSON)})
		require.NoError(t, err)
		require.NoError(t, txn.Commit(ctx))

		for _, test := range cases {
			txn := client.NewTxn()
			reply, err := txn.Query(ctx, test.query)
			require.NoError(t, err)
			testutil.CompareJSON(t, test.wantResult, string(reply.GetJson()))
		}
	}

	suite(
		"name: string @index(term) .",
		`[
			{ "name": "Michael" },
			{ "name": "Amit" },
			{ "name": "Luke" },
			{ "name": "Darth" },
			{ "name": "Sarah" },
			{ "name": "Ricky" },
			{ "name": "Hugo" }
		]`,
		[]testCase{
			{`
				{
					q(func: eq(name, "Hugo")) {
						name
					}
				}`, `
				{
				"q": [
				  {
					"name": "Hugo"
				  }
				]
			  }`,
			},
		},
	)
}

func TestHAClusterSetup(t *testing.T) {
	client := getClientForAlpha(t, "alpha1", "9180")
	runTests(t, client)
}

func TestHAClusterDiffClients(t *testing.T) {
	client := getClientForAlpha(t, "alpha1", "9180")
	client2 := getClientForAlpha(t, "alpha2", "9280")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	require.NoError(t, client.Alter(ctx, &api.Operation{
		DropAll: true,
	}))
	require.NoError(t, client.Alter(ctx, &api.Operation{
		Schema: "name: string @index(term) .",
	}))

	txn := client.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{SetJson: []byte(`[
			{ "name": "Michael" },
			{ "name": "Amit" },
			{ "name": "Luke" },
			{ "name": "Darth" },
			{ "name": "Sarah" },
			{ "name": "Ricky" },
			{ "name": "Hugo" }
		]`)})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	txn = client2.NewTxn()
	reply, err := txn.Query(ctx, `
				{
					q(func: eq(name, "Hugo")) {
						name
					}
				}`)
	require.NoError(t, err)
	testutil.CompareJSON(t, `
				{
				"q": [
				  {
					"name": "Hugo"
				  }
				]
			  }`, string(reply.GetJson()))
}

func getClientForAlpha(t *testing.T, name string, port string) *dgo.Dgraph {
	c := &x.TLSHelperConfig{
		CertDir:          "../tls/" + name,
		CertRequired:     true,
		Cert:             "../tls/" + name + "/client." + name + ".crt",
		Key:              "../tls/" + name + "/client." + name + ".key",
		ServerName:       name,
		RootCACert:       "../tls/" + name + "/ca.crt",
		UseSystemCACerts: true,
	}
	tlsConf, err := x.GenerateClientTLSConfig(c)
	require.NoError(t, err)
	dgConn, err := grpc.Dial(":" + port, grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)))
	require.NoError(t, err)
	client := dgo.NewDgraphClient(api.NewDgraphClient(dgConn))
	return client
}