package opa

import (
	"context"
	"log"
	"testing"

	pb "entrogo.com/entroq/proto"
)

func TestOPA_Authorize_Basic(t *testing.T) {
	opa, err := New(WithPolicy(`
	package entroq.authz

	entities = {
		"users": [
			{
				"name": "chris",
				"queues": [
					{
						"prefix": "/ns=chris/",
						"actions": ["ANY"]
					},
					{
						"exact": "/global/inbox",
						"actions": ["INSERT"]
					},
				]
			}
		],
		"roles": [
			{
				"name": "*",
				"queues": [
					{
						"exact": "/global/config",
						"actions": ["READ"]
					}
				]
			},
			{
				"name": "admin",
				"queues": [
					{
						"prefix": "",
						"actions": ["ANY"]
					}
				]
			}
		]
	}

	# Rules

	public_roles[role] {
		role := entities.roles[_]
		role.name == "*"
	}

	# A queue is public if its name matches exactly in a public role queue.
	public_queue[q] {
		some q
		public_roles[_].queues[_].exact == q.exact
	}

	# A queue is also public if its name matches a prefix in a public role queue.
	public_queue[q] {
		some q
		startswith(q.exact, public_roles.queues[_].prefix)
	}

	# A queue is also public if its prefix falls within a public prefix.
	public_queue[q] {
		some q
		startswith(q.prefix, public_roles[_].queues[_].prefix)
	}

	# Queue allowed if it is in the public set and has a matching action.
	queue_allowed[q] {
		qs := public_queue[q]
	}


	`))
	if err != nil {
		t.Fatalf("Error creating OPA: %v", err)
	}
	defer opa.Close()

	toAuthorize := &pb.AuthzRequest{
		Authz: &pb.Authz{
			Token: "chris",
		},
		Queues: []*pb.AuthzQueue{
			{
				Exact:   "/ns=chris/myinbox",
				Actions: []pb.AuthzQueue_Action{pb.AuthzQueue_CLAIM},
			},
		},
	}

	ctx := context.Background()
	resp, err := opa.Authorize(ctx, toAuthorize)
	if err != nil {
		t.Fatalf("Error authorizing: %v", err)
	}

	log.Println(resp)
}
