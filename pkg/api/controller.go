package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-openapi/swag"
	"github.com/gorilla/sessions"
	"github.com/treeverse/lakefs/pkg/actions"
	"github.com/treeverse/lakefs/pkg/api/apigen"
	"github.com/treeverse/lakefs/pkg/api/apiutil"
	"github.com/treeverse/lakefs/pkg/auth"
	"github.com/treeverse/lakefs/pkg/auth/acl"
	"github.com/treeverse/lakefs/pkg/auth/email"
	"github.com/treeverse/lakefs/pkg/auth/model"
	"github.com/treeverse/lakefs/pkg/auth/setup"
	"github.com/treeverse/lakefs/pkg/block"
	"github.com/treeverse/lakefs/pkg/catalog"
	"github.com/treeverse/lakefs/pkg/cloud"
	"github.com/treeverse/lakefs/pkg/config"
	"github.com/treeverse/lakefs/pkg/graveler"
	"github.com/treeverse/lakefs/pkg/graveler/ref"
	"github.com/treeverse/lakefs/pkg/httputil"
	"github.com/treeverse/lakefs/pkg/kv"
	"github.com/treeverse/lakefs/pkg/logging"
	"github.com/treeverse/lakefs/pkg/permissions"
	tablediff "github.com/treeverse/lakefs/pkg/plugins/diff"
	"github.com/treeverse/lakefs/pkg/samplerepo"
	"github.com/treeverse/lakefs/pkg/stats"
	"github.com/treeverse/lakefs/pkg/templater"
	"github.com/treeverse/lakefs/pkg/upload"
	"github.com/treeverse/lakefs/pkg/validator"
	"github.com/treeverse/lakefs/pkg/version"
)

const (
	// DefaultMaxPerPage is the maximum amount of results returned for paginated queries to the API
	DefaultMaxPerPage int = 1000
	lakeFSPrefix          = "symlinks"

	actionStatusCompleted = "completed"
	actionStatusFailed    = "failed"
	actionStatusSkipped   = "skipped"

	entryTypeObject       = "object"
	entryTypeCommonPrefix = "common_prefix"

	DefaultMaxDeleteObjects = 1000

	DefaultResetPasswordExpiration = 20 * time.Minute

	// httpStatusClientClosedRequest used as internal status code when request context is cancelled
	httpStatusClientClosedRequest = 499
	// httpStatusClientClosedRequestText text used for client closed request status code
	httpStatusClientClosedRequestText = "Client closed request"
)

type actionsHandler interface {
	GetRunResult(ctx context.Context, repositoryID, runID string) (*actions.RunResult, error)
	GetTaskResult(ctx context.Context, repositoryID, runID, hookRunID string) (*actions.TaskResult, error)
	ListRunResults(ctx context.Context, repositoryID, branchID, commitID, after string) (actions.RunResultIterator, error)
	ListRunTaskResults(ctx context.Context, repositoryID, runID, after string) (actions.TaskResultIterator, error)
}

type Migrator interface {
	Migrate(ctx context.Context) error
}

type Controller struct {
	Config                *config.Config
	Catalog               catalog.Interface
	Authenticator         auth.Authenticator
	Auth                  auth.Service
	BlockAdapter          block.Adapter
	MetadataManager       auth.MetadataManager
	Migrator              Migrator
	Collector             stats.Collector
	CloudMetadataProvider cloud.MetadataProvider
	Actions               actionsHandler
	AuditChecker          AuditChecker
	Logger                logging.Logger
	Emailer               *email.Emailer
	Templater             templater.Service
	sessionStore          sessions.Store
	PathProvider          upload.PathProvider
	otfDiffService        *tablediff.Service
}

func (c *Controller) PrepareGarbageCollectionUncommitted(w http.ResponseWriter, r *http.Request, body apigen.PrepareGarbageCollectionUncommittedJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.PrepareGarbageCollectionUncommittedAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "prepare_garbage_collection_uncommitted", r, repository, "", "")

	continuationToken := apiutil.Value(body.ContinuationToken)
	mark, err := decodeGCUncommittedMark(continuationToken)
	if err != nil {
		c.Logger.WithError(err).
			WithFields(logging.Fields{"repository": repository, "continuation_token": continuationToken}).
			Error("Failed to decode gc uncommitted continuation token")
		writeError(w, r, http.StatusBadRequest, "invalid continuation token")
		return
	}

	uncommittedInfo, err := c.Catalog.PrepareGCUncommitted(ctx, repository, mark)
	if err != nil {
		c.Logger.WithError(err).
			WithFields(logging.Fields{"repository": repository, "continuation_token": continuationToken}).
			Error("PrepareGCUncommitted failed")
		c.handleAPIError(ctx, w, r, err)
		return
	}

	nextContinuationToken, err := encodeGCUncommittedMark(uncommittedInfo.Mark)
	if err != nil {
		c.Logger.WithError(err).
			WithFields(logging.Fields{
				"repository": repository,
				"mark":       spew.Sdump(uncommittedInfo.Mark),
			}).
			Error("Failed encoding uncommitted gc mark")
		writeError(w, r, http.StatusInternalServerError, "failed to encode uncommitted mark")
		return
	}

	writeResponse(w, r, http.StatusCreated, apigen.PrepareGCUncommittedResponse{
		RunId:                 uncommittedInfo.RunID,
		GcUncommittedLocation: uncommittedInfo.Location,
		ContinuationToken:     nextContinuationToken,
	})
}

func (c *Controller) GetAuthCapabilities(w http.ResponseWriter, r *http.Request) {
	inviteSupported := c.Auth.IsInviteSupported()
	emailSupported := c.Emailer.Params.SMTPHost != ""
	writeResponse(w, r, http.StatusOK, apigen.AuthCapabilities{
		InviteUser:     &inviteSupported,
		ForgotPassword: &emailSupported,
	})
}

func (c *Controller) DeleteObjects(w http.ResponseWriter, r *http.Request, body apigen.DeleteObjectsJSONRequestBody, repository, branch string) {
	ctx := r.Context()
	c.LogAction(ctx, "delete_objects", r, repository, branch, "")

	// limit check
	if len(body.Paths) > DefaultMaxDeleteObjects {
		err := fmt.Errorf("%w, max paths is set to %d", ErrRequestSizeExceeded, DefaultMaxDeleteObjects)
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	// errs used to collect errors as part of the response, can't be nil
	errs := make([]apigen.ObjectError, 0)
	// check if we authorize to delete each object, prepare a list of paths we can delete
	var pathsToDelete []string
	for _, objectPath := range body.Paths {
		if !c.authorize(w, r, permissions.Node{
			Permission: permissions.Permission{
				Action:   permissions.DeleteObjectAction,
				Resource: permissions.ObjectArn(repository, objectPath),
			},
		}) {
			errs = append(errs, apigen.ObjectError{
				Path:       apiutil.Ptr(objectPath),
				StatusCode: http.StatusUnauthorized,
				Message:    http.StatusText(http.StatusUnauthorized),
			})
		} else {
			pathsToDelete = append(pathsToDelete, objectPath)
		}
	}

	// batch delete the entries we allow to delete
	delErr := c.Catalog.DeleteEntries(ctx, repository, branch, pathsToDelete)
	delErrs := graveler.NewMapDeleteErrors(delErr)
	for _, objectPath := range pathsToDelete {
		// set err to the specific error when possible
		err := delErrs[objectPath]
		if err == nil {
			err = delErr
		}
		lg := c.Logger.WithField("path", objectPath)
		switch {
		case errors.Is(err, graveler.ErrNotFound):
			lg.WithError(err).Debug("tried to delete a non-existent object")
		case errors.Is(err, graveler.ErrWriteToProtectedBranch):
			errs = append(errs, apigen.ObjectError{
				Path:       apiutil.Ptr(objectPath),
				StatusCode: http.StatusForbidden,
				Message:    err.Error(),
			})
		case errors.Is(err, catalog.ErrPathRequiredValue):
			// issue #1706 - https://github.com/treeverse/lakeFS/issues/1706
			// Spark trying to delete the path "main/", which we map to branch "main" with an empty path.
			// Spark expects it to succeed (not deleting anything is a success), instead of returning an error.
			lg.Debug("tried to delete with an empty branch")
		case err != nil:
			lg.WithError(err).Error("failed deleting object")
			errs = append(errs, apigen.ObjectError{
				Path:       apiutil.Ptr(objectPath),
				StatusCode: http.StatusInternalServerError,
				Message:    err.Error(),
			})
		default:
			lg.Debug("object set for deletion")
		}
	}

	response := apigen.ObjectErrorList{
		Errors: errs,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) Login(w http.ResponseWriter, r *http.Request, body apigen.LoginJSONRequestBody) {
	ctx := r.Context()
	user, err := userByAuth(ctx, c.Logger, c.Authenticator, c.Auth, body.AccessKeyId, body.SecretAccessKey)
	if errors.Is(err, ErrAuthenticatingRequest) {
		writeResponse(w, r, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		return
	}

	loginTime := time.Now()
	duration := c.Config.Auth.LoginDuration
	expires := loginTime.Add(duration)
	secret := c.Auth.SecretStore().SharedSecret()

	tokenString, err := GenerateJWTLogin(secret, user.Username, loginTime, expires)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return
	}
	internalAuthSession, _ := c.sessionStore.Get(r, InternalAuthSessionName)
	internalAuthSession.Values[TokenSessionKeyName] = tokenString
	err = c.sessionStore.Save(r, w, internalAuthSession)
	if err != nil {
		c.Logger.WithError(err).Error("Failed to save internal auth session")
		writeError(w, r, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return
	}
	response := apigen.AuthenticationToken{
		Token:           tokenString,
		TokenExpiration: apiutil.Ptr(expires.Unix()),
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) GetPhysicalAddress(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.GetPhysicalAddressParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.WriteObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "generate_physical_address", r, repository, branch, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if errors.Is(err, graveler.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	token, err := c.Catalog.GetStagingToken(ctx, repository, branch)
	if errors.Is(err, graveler.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	address := c.PathProvider.NewPath()
	qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, address, block.IdentifierTypeRelative)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	err = c.Catalog.SetLinkAddress(ctx, repository, address)
	if err != nil {
		c.handleAPIError(ctx, w, r, err)
		return
	}

	response := &apigen.StagingLocation{
		PhysicalAddress: apiutil.Ptr(qk.Format()),
		Token:           apiutil.Value(token),
	}

	if swag.BoolValue(params.Presign) {
		// generate a pre-signed PUT url for the given request
		preSignedURL, expiry, err := c.BlockAdapter.GetPreSignedURL(ctx, block.ObjectPointer{
			StorageNamespace: repo.StorageNamespace,
			Identifier:       address,
			IdentifierType:   block.IdentifierTypeRelative,
		}, block.PreSignModeWrite)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		response.PresignedUrl = &preSignedURL
		if !expiry.IsZero() {
			response.PresignedUrlExpiry = apiutil.Ptr(expiry.Unix())
		}
	}

	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) LinkPhysicalAddress(w http.ResponseWriter, r *http.Request, body apigen.LinkPhysicalAddressJSONRequestBody, repository, branch string, params apigen.LinkPhysicalAddressParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.WriteObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "stage_object", r, repository, branch, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if errors.Is(err, graveler.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	// write metadata
	qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, params.Path, block.IdentifierTypeRelative)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	blockStoreType := c.BlockAdapter.BlockstoreType()
	expectedType := qk.GetStorageType().BlockstoreType()
	if expectedType != blockStoreType {
		c.Logger.WithContext(ctx).WithFields(logging.Fields{
			"expected_type":   expectedType,
			"blockstore_type": blockStoreType,
		}).Error("invalid blockstore type")
		c.handleAPIError(ctx, w, r, fmt.Errorf("invalid blockstore type: %w", block.ErrInvalidAddress))
		return
	}

	writeTime := time.Now()
	physicalAddress, addressType := normalizePhysicalAddress(repo.StorageNamespace, apiutil.Value(body.Staging.PhysicalAddress))

	// validate token
	err = c.Catalog.VerifyLinkAddress(ctx, repository, physicalAddress)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// Because CreateEntry tracks staging on a database with atomic operations,
	// _ignore_ the staging token here: no harm done even if a race was lost
	// against a commit.
	entryBuilder := catalog.NewDBEntryBuilder().
		CommonLevel(false).
		Path(params.Path).
		PhysicalAddress(physicalAddress).
		AddressType(addressType).
		CreationDate(writeTime).
		Size(body.SizeBytes).
		Checksum(body.Checksum).
		ContentType(apiutil.Value(body.ContentType))
	if body.UserMetadata != nil {
		entryBuilder.Metadata(body.UserMetadata.AdditionalProperties)
	}
	entry := entryBuilder.Build()

	err = c.Catalog.CreateEntry(ctx, repo.Name, branch, entry)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	metadata := apigen.ObjectUserMetadata{AdditionalProperties: entry.Metadata}
	response := apigen.ObjectStats{
		Checksum:        entry.Checksum,
		ContentType:     &entry.ContentType,
		Metadata:        &metadata,
		Mtime:           entry.CreationDate.Unix(),
		Path:            entry.Path,
		PathType:        entryTypeObject,
		PhysicalAddress: entry.PhysicalAddress,
		SizeBytes:       apiutil.Ptr(entry.Size),
	}

	writeResponse(w, r, http.StatusOK, response)
}

// normalizePhysicalAddress return relative address based on storage namespace if possible. If address doesn't match
// the storage namespace prefix, the return address type is full.
func normalizePhysicalAddress(storageNamespace, physicalAddress string) (string, catalog.AddressType) {
	prefix := storageNamespace
	if !strings.HasSuffix(prefix, catalog.DefaultPathDelimiter) {
		prefix += catalog.DefaultPathDelimiter
	}
	if strings.HasPrefix(physicalAddress, prefix) {
		return physicalAddress[len(prefix):], catalog.AddressTypeRelative
	}
	return physicalAddress, catalog.AddressTypeFull
}

func (c *Controller) ListGroups(w http.ResponseWriter, r *http.Request, params apigen.ListGroupsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListGroupsAction,
			Resource: permissions.All,
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "list_groups", r, "", "", "")
	groups, paginator, err := c.Auth.ListGroups(ctx, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.GroupList{
		Results: make([]apigen.Group, 0, len(groups)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}

	for _, g := range groups {
		response.Results = append(response.Results, apigen.Group{
			Id:           g.DisplayName,
			CreationDate: g.CreatedAt.Unix(),
		})
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) CreateGroup(w http.ResponseWriter, r *http.Request, body apigen.CreateGroupJSONRequestBody) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateGroupAction,
			Resource: permissions.GroupArn(body.Id),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_group", r, "", "", "")

	// Check that group name is valid
	valid, msg := c.isNameValid(body.Id, "Group")
	if !valid {
		writeError(w, r, http.StatusBadRequest, msg)
		return
	}

	g := &model.Group{
		CreatedAt:   time.Now().UTC(),
		DisplayName: body.Id,
	}
	err := c.Auth.CreateGroup(ctx, g)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.Group{
		CreationDate: g.CreatedAt.Unix(),
		Id:           g.DisplayName,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) DeleteGroup(w http.ResponseWriter, r *http.Request, groupID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteGroupAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "delete_group", r, "", "", "")
	err := c.Auth.DeleteGroup(ctx, groupID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "group not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetGroup(w http.ResponseWriter, r *http.Request, groupID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadGroupAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_group", r, "", "", "")
	g, err := c.Auth.GetGroup(ctx, groupID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "group not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.Group{
		Id:           g.DisplayName,
		CreationDate: g.CreatedAt.Unix(),
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) GetGroupACL(w http.ResponseWriter, r *http.Request, groupID string) {
	aclPolicyName := acl.ACLPolicyName(groupID)
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadGroupAction,
					Resource: permissions.GroupArn(groupID),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadPolicyAction,
					Resource: permissions.PolicyArn(aclPolicyName),
				},
			},
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "get_group_acl", r, "", "", "")
	policies, _, err := c.Auth.ListGroupPolicies(ctx, groupID, &model.PaginationParams{
		Amount: 2, //nolint:gomnd
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	var groupACL model.ACL
	switch len(policies) {
	case 0: // Blank ACL is valid and allows nothing
		break
	case 1:
		groupACL = policies[0].ACL
		if len(groupACL.Permission) == 0 {
			c.Logger.
				WithContext(ctx).
				WithField("policy", fmt.Sprintf("%+v", policies[0])).
				WithField("acl", fmt.Sprintf("%+v", groupACL)).
				WithField("group", groupID).
				Warn("Policy attached to group has no ACL")
			response := apigen.NotFoundOrNoACL{
				Message: "Policy attached to group has no ACL",
				NoAcl:   swag.Bool(true),
			}
			writeResponse(w, r, http.StatusNotFound, response)
			return
		}
	default:
		c.Logger.
			WithContext(ctx).
			WithField("num_policies", len(policies)).
			WithField("group", groupID).
			Warn("Wrong number of policies found")
		response := apigen.NotFoundOrNoACL{
			Message: "Multiple policies attached to group - no ACL",
			NoAcl:   swag.Bool(true),
		}
		writeResponse(w, r, http.StatusNotFound, response)
		return
	}

	response := apigen.ACL{
		Permission: string(groupACL.Permission),
	}

	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) SetGroupACL(w http.ResponseWriter, r *http.Request, body apigen.SetGroupACLJSONRequestBody, groupID string) {
	aclPolicyName := acl.ACLPolicyName(groupID)
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadGroupAction,
					Resource: permissions.GroupArn(groupID),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.AttachPolicyAction,
					Resource: permissions.PolicyArn(aclPolicyName),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.UpdatePolicyAction,
					Resource: permissions.PolicyArn(aclPolicyName),
				},
			},
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "set_group_acl", r, "", "", "")

	newACL := model.ACL{
		Permission: model.ACLPermission(body.Permission),
	}

	err := acl.WriteGroupACL(ctx, c.Auth, groupID, newACL, time.Now(), true)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	writeResponse(w, r, http.StatusCreated, nil)
}

func (c *Controller) ListGroupMembers(w http.ResponseWriter, r *http.Request, groupID string, params apigen.ListGroupMembersParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadGroupAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_group_users", r, "", "", "")
	users, paginator, err := c.Auth.ListGroupUsers(ctx, groupID, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.UserList{
		Results: make([]apigen.User, 0, len(users)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}
	for _, u := range users {
		response.Results = append(response.Results, apigen.User{
			Id:           u.Username,
			CreationDate: u.CreatedAt.Unix(),
			Email:        u.Email,
		})
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) DeleteGroupMembership(w http.ResponseWriter, r *http.Request, groupID, userID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.RemoveGroupMemberAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "remove_user_from_group", r, "", "", "")
	err := c.Auth.RemoveUserFromGroup(ctx, userID, groupID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) AddGroupMembership(w http.ResponseWriter, r *http.Request, groupID, userID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.AddGroupMemberAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "add_user_to_group", r, "", "", "")
	err := c.Auth.AddUserToGroup(ctx, userID, groupID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusCreated, nil)
}

func (c *Controller) ListGroupPolicies(w http.ResponseWriter, r *http.Request, groupID string, params apigen.ListGroupPoliciesParams) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadGroupAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "list_group_policies", r, "", "", "")
	policies, paginator, err := c.Auth.ListGroupPolicies(ctx, groupID, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.PolicyList{
		Results: make([]apigen.Policy, 0, len(policies)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}
	for _, p := range policies {
		response.Results = append(response.Results, serializePolicy(p))
	}

	writeResponse(w, r, http.StatusOK, response)
}

func serializePolicy(p *model.Policy) apigen.Policy {
	stmts := make([]apigen.Statement, 0, len(p.Statement))
	for _, s := range p.Statement {
		stmts = append(stmts, apigen.Statement{
			Action:   s.Action,
			Effect:   s.Effect,
			Resource: s.Resource,
		})
	}
	createdAt := p.CreatedAt.Unix()
	return apigen.Policy{
		Id:           p.DisplayName,
		CreationDate: &createdAt, // TODO(barak): check if CreationDate should be required
		Statement:    stmts,
	}
}

func (c *Controller) DetachPolicyFromGroup(w http.ResponseWriter, r *http.Request, groupID, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DetachPolicyAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "detach_policy_from_group", r, "", "", "")
	err := c.Auth.DetachPolicyFromGroup(ctx, policyID, groupID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) AttachPolicyToGroup(w http.ResponseWriter, r *http.Request, groupID, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.AttachPolicyAction,
			Resource: permissions.GroupArn(groupID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "attach_policy_to_group", r, "", "", "")
	err := c.Auth.AttachPolicyToGroup(ctx, policyID, groupID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusCreated, nil)
}

func (c *Controller) ListPolicies(w http.ResponseWriter, r *http.Request, params apigen.ListPoliciesParams) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListPoliciesAction,
			Resource: permissions.All,
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "list_policies", r, "", "", "")
	policies, paginator, err := c.Auth.ListPolicies(ctx, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.PolicyList{
		Results: make([]apigen.Policy, 0, len(policies)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}
	for _, p := range policies {
		response.Results = append(response.Results, serializePolicy(p))
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) CreatePolicy(w http.ResponseWriter, r *http.Request, body apigen.CreatePolicyJSONRequestBody) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreatePolicyAction,
			Resource: permissions.PolicyArn(body.Id),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_policy", r, "", "", "")

	// Check that policy ID is valid
	valid, msg := c.isNameValid(body.Id, "Policy")
	if !valid {
		writeError(w, r, http.StatusBadRequest, msg)
		return
	}

	stmts := make(model.Statements, len(body.Statement))
	for i, apiStatement := range body.Statement {
		stmts[i] = model.Statement{
			Effect:   apiStatement.Effect,
			Action:   apiStatement.Action,
			Resource: apiStatement.Resource,
		}
	}

	p := &model.Policy{
		CreatedAt:   time.Now().UTC(),
		DisplayName: body.Id,
		Statement:   stmts,
	}

	err := c.Auth.WritePolicy(ctx, p, false)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	writeResponse(w, r, http.StatusCreated, serializePolicy(p))
}

func (c *Controller) DeletePolicy(w http.ResponseWriter, r *http.Request, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeletePolicyAction,
			Resource: permissions.PolicyArn(policyID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "delete_policy", r, "", "", "")
	err := c.Auth.DeletePolicy(ctx, policyID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "policy not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetPolicy(w http.ResponseWriter, r *http.Request, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadPolicyAction,
			Resource: permissions.PolicyArn(policyID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_policy", r, "", "", "")
	p, err := c.Auth.GetPolicy(ctx, policyID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "policy not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := serializePolicy(p)
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) UpdatePolicy(w http.ResponseWriter, r *http.Request, body apigen.UpdatePolicyJSONRequestBody, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.UpdatePolicyAction,
			Resource: permissions.PolicyArn(policyID),
		},
	}) {
		return
	}
	// verify requested policy match the policy document id
	if policyID != body.Id {
		writeError(w, r, http.StatusBadRequest, "can't update policy id")
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "update_policy", r, "", "", "")

	stmts := make(model.Statements, len(body.Statement))
	for i, apiStatement := range body.Statement {
		stmts[i] = model.Statement{
			Effect:   apiStatement.Effect,
			Action:   apiStatement.Action,
			Resource: apiStatement.Resource,
		}
	}

	p := &model.Policy{
		CreatedAt:   time.Now().UTC(),
		DisplayName: policyID,
		Statement:   stmts,
	}
	err := c.Auth.WritePolicy(ctx, p, true)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := serializePolicy(p)
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) ListUsers(w http.ResponseWriter, r *http.Request, params apigen.ListUsersParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListUsersAction,
			Resource: permissions.All,
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_users", r, "", "", "")
	users, paginator, err := c.Auth.ListUsers(ctx, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.UserList{
		Results: make([]apigen.User, 0, len(users)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}
	for _, u := range users {
		response.Results = append(response.Results, apigen.User{
			Id:           u.Username,
			CreationDate: u.CreatedAt.Unix(),
			Email:        u.Email,
		})
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) generateResetPasswordToken(email string, duration time.Duration) (string, error) {
	secret := c.Auth.SecretStore().SharedSecret()
	currentTime := time.Now()
	return auth.GenerateJWTResetPassword(secret, email, currentTime, currentTime.Add(duration))
}

func (c *Controller) CreateUser(w http.ResponseWriter, r *http.Request, body apigen.CreateUserJSONRequestBody) {
	invite := swag.BoolValue(body.InviteUser)
	username := body.Id

	// Check that username is valid
	valid, msg := c.isNameValid(username, "User")
	if !valid {
		writeError(w, r, http.StatusBadRequest, msg)
		return
	}

	var parsedEmail *string
	if invite {
		addr, err := mail.ParseAddress(username)
		if err != nil {
			c.Logger.WithError(err).WithField("user_id", username).Warn("failed parsing email")
			writeError(w, r, http.StatusBadRequest, "Invalid email format")
			return
		}
		username = strings.ToLower(addr.Address)
		parsedEmail = &addr.Address
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateUserAction,
			Resource: permissions.UserArn(username),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_user", r, "", "", "")
	if invite {
		err := c.Auth.InviteUser(ctx, *parsedEmail)
		if c.handleAPIError(ctx, w, r, err) {
			c.Logger.WithError(err).WithField("email", *parsedEmail).Warn("failed creating user")
			return
		}
		writeResponse(w, r, http.StatusCreated, apigen.User{Id: *parsedEmail})
		return
	}
	u := &model.User{
		CreatedAt:    time.Now().UTC(),
		Username:     username,
		FriendlyName: nil,
		Source:       "internal",
		Email:        parsedEmail,
	}

	_, err := c.Auth.CreateUser(ctx, u)
	if c.handleAPIError(ctx, w, r, err) {
		c.Logger.WithError(err).WithField("username", u.Username).Warn("failed creating user")
		return
	}
	response := apigen.User{
		Id:           u.Username,
		CreationDate: u.CreatedAt.Unix(),
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) DeleteUser(w http.ResponseWriter, r *http.Request, userID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteUserAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "delete_user", r, "", "", "")
	err := c.Auth.DeleteUser(ctx, userID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "user not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetUser(w http.ResponseWriter, r *http.Request, userID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadUserAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_user", r, "", "", "")
	u, err := c.Auth.GetUser(ctx, userID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "user not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.User{
		CreationDate: u.CreatedAt.Unix(),
		Id:           u.Username,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) ListUserCredentials(w http.ResponseWriter, r *http.Request, userID string, params apigen.ListUserCredentialsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListCredentialsAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_user_credentials", r, "", "", "")
	credentials, paginator, err := c.Auth.ListUserCredentials(ctx, userID, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.CredentialsList{
		Results: make([]apigen.Credentials, 0, len(credentials)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}
	for _, c := range credentials {
		response.Results = append(response.Results, apigen.Credentials{
			AccessKeyId:  c.AccessKeyID,
			CreationDate: c.IssuedDate.Unix(),
		})
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) CreateCredentials(w http.ResponseWriter, r *http.Request, userID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateCredentialsAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_credentials", r, "", "", "")
	credentials, err := c.Auth.CreateCredentials(ctx, userID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.CredentialsWithSecret{
		AccessKeyId:     credentials.AccessKeyID,
		SecretAccessKey: credentials.SecretAccessKey,
		CreationDate:    credentials.IssuedDate.Unix(),
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) DeleteCredentials(w http.ResponseWriter, r *http.Request, userID, accessKeyID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteCredentialsAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "delete_credentials", r, "", "", "")
	err := c.Auth.DeleteCredentials(ctx, userID, accessKeyID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "credentials not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetCredentials(w http.ResponseWriter, r *http.Request, userID, accessKeyID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadCredentialsAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_credentials_for_user", r, "", "", "")
	credentials, err := c.Auth.GetCredentialsForUser(ctx, userID, accessKeyID)
	if errors.Is(err, auth.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "credentials not found")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.Credentials{
		AccessKeyId:  credentials.AccessKeyID,
		CreationDate: credentials.IssuedDate.Unix(),
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) ListUserGroups(w http.ResponseWriter, r *http.Request, userID string, params apigen.ListUserGroupsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadUserAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_user_groups", r, "", "", "")
	groups, paginator, err := c.Auth.ListUserGroups(ctx, userID, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.GroupList{
		Results: make([]apigen.Group, 0, len(groups)),
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
	}
	for _, g := range groups {
		response.Results = append(response.Results, apigen.Group{
			Id:           g.DisplayName,
			CreationDate: g.CreatedAt.Unix(),
		})
	}

	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) ListUserPolicies(w http.ResponseWriter, r *http.Request, userID string, params apigen.ListUserPoliciesParams) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadUserAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "list_user_policies", r, "", "", "")
	var listPolicies func(ctx context.Context, username string, params *model.PaginationParams) ([]*model.Policy, *model.Paginator, error)
	if params.Effective != nil && *params.Effective {
		listPolicies = c.Auth.ListEffectivePolicies
	} else {
		listPolicies = c.Auth.ListUserPolicies
	}
	policies, paginator, err := listPolicies(ctx, userID, &model.PaginationParams{
		After:  paginationAfter(params.After),
		Prefix: paginationPrefix(params.Prefix),
		Amount: paginationAmount(params.Amount),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.PolicyList{
		Pagination: apigen.Pagination{
			HasMore:    paginator.NextPageToken != "",
			NextOffset: paginator.NextPageToken,
			Results:    paginator.Amount,
		},
		Results: make([]apigen.Policy, 0, len(policies)),
	}
	for _, p := range policies {
		response.Results = append(response.Results, serializePolicy(p))
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) DetachPolicyFromUser(w http.ResponseWriter, r *http.Request, userID, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DetachPolicyAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "detach_policy_from_user", r, "", "", "")
	err := c.Auth.DetachPolicyFromUser(ctx, policyID, userID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) AttachPolicyToUser(w http.ResponseWriter, r *http.Request, userID, policyID string) {
	if c.Config.IsAuthUISimplified() {
		writeError(w, r, http.StatusNotImplemented, "Not implemented")
		return
	}
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.AttachPolicyAction,
			Resource: permissions.UserArn(userID),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "attach_policy_to_user", r, "", "", "")
	err := c.Auth.AttachPolicyToUser(ctx, policyID, userID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusCreated, nil)
}

func (c *Controller) GetStorageConfig(w http.ResponseWriter, r *http.Request) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadConfigAction,
			Resource: permissions.All,
		},
	}) {
		return
	}
	info := c.BlockAdapter.GetStorageNamespaceInfo()
	defaultNamespacePrefix := swag.String(info.DefaultNamespacePrefix)
	if c.Config.Blockstore.DefaultNamespacePrefix != nil {
		defaultNamespacePrefix = c.Config.Blockstore.DefaultNamespacePrefix
	}
	response := apigen.StorageConfig{
		BlockstoreType:                   c.Config.Blockstore.Type,
		BlockstoreNamespaceValidityRegex: info.ValidityRegex,
		BlockstoreNamespaceExample:       info.Example,
		DefaultNamespacePrefix:           defaultNamespacePrefix,
		PreSignSupport:                   info.PreSignSupport,
		PreSignSupportUi:                 info.PreSignSupportUI,
		ImportSupport:                    info.ImportSupport,
		ImportValidityRegex:              info.ImportValidityRegex,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) HealthCheck(w http.ResponseWriter, r *http.Request) {
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) ListRepositories(w http.ResponseWriter, r *http.Request, params apigen.ListRepositoriesParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListRepositoriesAction,
			Resource: permissions.All,
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_repos", r, "", "", "")

	repos, hasMore, err := c.Catalog.ListRepositories(ctx, paginationAmount(params.Amount), paginationPrefix(params.Prefix), paginationAfter(params.After))
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	results := make([]apigen.Repository, 0, len(repos))
	for _, repo := range repos {
		creationDate := repo.CreationDate.Unix()
		r := apigen.Repository{
			Id:               repo.Name,
			StorageNamespace: repo.StorageNamespace,
			CreationDate:     creationDate,
			DefaultBranch:    repo.DefaultBranch,
		}
		results = append(results, r)
	}
	repositoryList := apigen.RepositoryList{
		Pagination: paginationFor(hasMore, results, "Id"),
		Results:    results,
	}
	writeResponse(w, r, http.StatusOK, repositoryList)
}

func (c *Controller) CreateRepository(w http.ResponseWriter, r *http.Request, body apigen.CreateRepositoryJSONRequestBody, params apigen.CreateRepositoryParams) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateRepositoryAction,
					Resource: permissions.RepoArn(body.Name),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.AttachStorageNamespaceAction,
					Resource: permissions.StorageNamespace(body.StorageNamespace),
				},
			},
		},
	}) {
		return
	}
	ctx := r.Context()
	sampleData := swag.BoolValue(body.SampleData)
	c.LogAction(ctx, "create_repo", r, body.Name, "", "")
	if sampleData {
		c.LogAction(ctx, "repo_sample_data", r, body.Name, "", "")
	}

	if err := c.validateStorageNamespace(body.StorageNamespace); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	defaultBranch := apiutil.Value(body.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	if swag.BoolValue(params.Bare) {
		// create a bare repository. This is useful in conjunction with refs-restore to create a copy
		// of another repository by e.g. copying the _lakefs/ directory and restoring its refs
		repo, err := c.Catalog.CreateBareRepository(ctx, body.Name, body.StorageNamespace, defaultBranch)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		response := apigen.Repository{
			CreationDate:     repo.CreationDate.Unix(),
			DefaultBranch:    repo.DefaultBranch,
			Id:               repo.Name,
			StorageNamespace: repo.StorageNamespace,
		}
		writeResponse(w, r, http.StatusCreated, response)
		return
	}

	if err := c.ensureStorageNamespace(ctx, body.StorageNamespace); err != nil {
		var (
			reason string
			retErr error
			urlErr *url.Error
		)
		switch {
		case errors.As(err, &urlErr) && urlErr.Op == "parse":
			retErr = err
			reason = "bad_url"
		case errors.Is(err, block.ErrInvalidAddress):
			retErr = fmt.Errorf("%w, must match: %s", err, c.BlockAdapter.BlockstoreType())
			reason = "invalid_namespace"
		case errors.Is(err, ErrStorageNamespaceInUse):
			retErr = err
			reason = "already_in_use"
		default:
			retErr = ErrFailedToAccessStorage
			reason = "unknown"
		}
		c.Logger.
			WithError(err).
			WithField("storage_namespace", body.StorageNamespace).
			WithField("reason", reason).
			Warn("Could not access storage namespace")
		writeError(w, r, http.StatusBadRequest, fmt.Errorf("failed to create repository: %w", retErr))
		return
	}

	newRepo, err := c.Catalog.CreateRepository(ctx, body.Name, body.StorageNamespace, defaultBranch)
	if err != nil {
		c.handleAPIError(ctx, w, r, fmt.Errorf("error creating repository: %w", err))
		return
	}

	if sampleData {
		// add sample data, hooks, etc.
		user, err := auth.GetUser(ctx)
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "missing user")
			return
		}

		err = samplerepo.PopulateSampleRepo(ctx, newRepo, c.Catalog, c.PathProvider, c.BlockAdapter, user)
		if err != nil {
			c.handleAPIError(ctx, w, r, fmt.Errorf("error populating sample repository: %w", err))
			return
		}

		err = samplerepo.SampleRepoAddBranchProtection(ctx, newRepo, c.Catalog)
		if err != nil {
			c.handleAPIError(ctx, w, r, fmt.Errorf("error adding branch protection to sample repository: %w", err))
			return
		}
	}

	response := apigen.Repository{
		CreationDate:     newRepo.CreationDate.Unix(),
		DefaultBranch:    newRepo.DefaultBranch,
		Id:               newRepo.Name,
		StorageNamespace: newRepo.StorageNamespace,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) validateStorageNamespace(storageNamespace string) error {
	validRegex := c.BlockAdapter.GetStorageNamespaceInfo().ValidityRegex
	storagePrefixRegex, err := regexp.Compile(validRegex)
	if err != nil {
		return fmt.Errorf("failed to compile validity regex %s: %w", validRegex, block.ErrInvalidNamespace)
	}
	if !storagePrefixRegex.MatchString(storageNamespace) {
		return fmt.Errorf("failed to match required regex %s: %w", validRegex, block.ErrInvalidNamespace)
	}
	return nil
}

func (c *Controller) ensureStorageNamespace(ctx context.Context, storageNamespace string) error {
	const (
		dummyData    = "this is dummy data - created by lakeFS to check accessibility"
		dummyObjName = "dummy"
	)
	dummyKey := fmt.Sprintf("%s/%s", c.Config.Committed.BlockStoragePrefix, dummyObjName)

	objLen := int64(len(dummyData))

	// check if the dummy file exist in the root of the storage namespace
	// this serves 2 purposes, first, we maintain safety check for older lakeFS version.
	// second, in scenarios where lakeFS shouldn't have access to the root namespace (i.e pre-sign URL only).
	if c.Config.Graveler.EnsureReadableRootNamespace {
		rootObj := block.ObjectPointer{
			StorageNamespace: storageNamespace,
			IdentifierType:   block.IdentifierTypeRelative,
			Identifier:       dummyObjName,
		}

		if s, err := c.BlockAdapter.Get(ctx, rootObj, objLen); err == nil {
			s.Close()
			return fmt.Errorf("found lakeFS objects in the storage namespace root(%s): %w",
				storageNamespace, ErrStorageNamespaceInUse)
		} else if !errors.Is(err, block.ErrDataNotFound) {
			return err
		}
	}

	// check if the dummy file exists
	obj := block.ObjectPointer{
		StorageNamespace: storageNamespace,
		IdentifierType:   block.IdentifierTypeRelative,
		Identifier:       dummyKey,
	}

	if s, err := c.BlockAdapter.Get(ctx, obj, objLen); err == nil {
		s.Close()
		return fmt.Errorf("found lakeFS objects in the storage namespace(%s) key(%s): %w",
			storageNamespace, obj.Identifier, ErrStorageNamespaceInUse)
	} else if !errors.Is(err, block.ErrDataNotFound) {
		return err
	}

	if err := c.BlockAdapter.Put(ctx, obj, objLen, strings.NewReader(dummyData), block.PutOpts{}); err != nil {
		return err
	}

	_, err := c.BlockAdapter.Get(ctx, obj, objLen)
	return err
}

func (c *Controller) DeleteRepository(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteRepositoryAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "delete_repo", r, repository, "", "")
	err := c.Catalog.DeleteRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetRepository(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadRepositoryAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_repo", r, repository, "", "")
	repo, err := c.Catalog.GetRepository(ctx, repository)
	switch {
	case err == nil:
		response := apigen.Repository{
			CreationDate:     repo.CreationDate.Unix(),
			DefaultBranch:    repo.DefaultBranch,
			Id:               repo.Name,
			StorageNamespace: repo.StorageNamespace,
		}
		writeResponse(w, r, http.StatusOK, response)

	case errors.Is(err, graveler.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "repository not found")

	case errors.Is(err, graveler.ErrRepositoryInDeletion):
		writeError(w, r, http.StatusGone, err)

	default:
		writeError(w, r, http.StatusInternalServerError, err)
	}
}

func (c *Controller) GetRepositoryMetadata(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadRepositoryAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_repo_metadata", r, repository, "", "")
	metadata, err := c.Catalog.GetRepositoryMetadata(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusOK, apigen.RepositoryMetadata{AdditionalProperties: metadata})
}

func (c *Controller) ListRepositoryRuns(w http.ResponseWriter, r *http.Request, repository string, params apigen.ListRepositoryRunsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadActionsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "actions_repository_runs", r, repository, "", "")

	_, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	branchName := apiutil.Value(params.Branch)
	commitID := apiutil.Value(params.Commit)
	runsIter, err := c.Actions.ListRunResults(ctx, repository, branchName, commitID, paginationAfter(params.After))
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	defer runsIter.Close()

	response := apigen.ActionRunList{
		Pagination: apigen.Pagination{
			MaxPerPage: DefaultMaxPerPage,
		},
		Results: make([]apigen.ActionRun, 0),
	}
	amount := paginationAmount(params.Amount)
	for len(response.Results) < amount && runsIter.Next() {
		val := runsIter.Value()
		response.Results = append(response.Results, runResultToActionRun(val))
	}
	response.Pagination.Results = len(response.Results)
	if runsIter.Next() {
		response.Pagination.HasMore = true
		if len(response.Results) > 0 {
			lastRun := response.Results[len(response.Results)-1]
			response.Pagination.NextOffset = lastRun.RunId
		}
	}
	if err := runsIter.Err(); err != nil {
		writeResponse(w, r, http.StatusInternalServerError, err)
		return
	}
	writeResponse(w, r, http.StatusOK, response)
}

func runResultToActionRun(val *actions.RunResult) apigen.ActionRun {
	runResult := apigen.ActionRun{
		Branch:    val.BranchID,
		CommitId:  val.CommitID,
		RunId:     val.RunID,
		StartTime: val.StartTime,
		EndTime:   &val.EndTime,
		EventType: val.EventType,
	}
	if val.Passed {
		runResult.Status = actionStatusCompleted
	} else {
		runResult.Status = actionStatusFailed
	}
	return runResult
}

func (c *Controller) GetRun(w http.ResponseWriter, r *http.Request, repository, runID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadActionsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "actions_get_run", r, repository, "", "")
	_, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	runResult, err := c.Actions.GetRunResult(ctx, repository, runID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	var status string
	if runResult.Passed {
		status = actionStatusCompleted
	} else {
		status = actionStatusFailed
	}
	response := apigen.ActionRun{
		RunId:     runResult.RunID,
		EventType: runResult.EventType,
		StartTime: runResult.StartTime,
		EndTime:   &runResult.EndTime,
		Status:    status,
		Branch:    runResult.BranchID,
		CommitId:  runResult.CommitID,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) ListRunHooks(w http.ResponseWriter, r *http.Request, repository, runID string, params apigen.ListRunHooksParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadActionsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "actions_list_run_hooks", r, repository, "", "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	tasksIter, err := c.Actions.ListRunTaskResults(ctx, repo.Name, runID, paginationAfter(params.After))
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	defer tasksIter.Close()

	response := apigen.HookRunList{
		Results: make([]apigen.HookRun, 0),
		Pagination: apigen.Pagination{
			MaxPerPage: DefaultMaxPerPage,
		},
	}
	amount := paginationAmount(params.Amount)
	for len(response.Results) < amount && tasksIter.Next() {
		val := tasksIter.Value()
		hookRun := apigen.HookRun{
			HookRunId: val.HookRunID,
			Action:    val.ActionName,
			HookId:    val.HookID,
			StartTime: val.StartTime,
			EndTime:   &val.EndTime,
		}
		switch {
		case val.Passed:
			hookRun.Status = actionStatusCompleted
		case val.StartTime.IsZero(): // assumes that database values are only stored after run is finished
			hookRun.Status = actionStatusSkipped
			hookRun.EndTime = nil
		default:
			hookRun.Status = actionStatusFailed
		}
		response.Results = append(response.Results, hookRun)
	}
	if tasksIter.Next() {
		response.Pagination.HasMore = true
		if len(response.Results) > 0 {
			lastHookRun := response.Results[len(response.Results)-1]
			response.Pagination.NextOffset = lastHookRun.HookRunId
		}
	}
	if err := tasksIter.Err(); err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	response.Pagination.Results = len(response.Results)
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) GetRunHookOutput(w http.ResponseWriter, r *http.Request, repository, runID, hookRunID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadActionsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "actions_run_hook_output", r, repository, "", "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	taskResult, err := c.Actions.GetTaskResult(ctx, repo.Name, runID, hookRunID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	if taskResult.StartTime.IsZero() { // skipped task
		writeResponse(w, r, http.StatusOK, nil)
		return
	}

	logPath := taskResult.LogPath()
	reader, err := c.BlockAdapter.Get(ctx, block.ObjectPointer{
		StorageNamespace: repo.StorageNamespace,
		IdentifierType:   block.IdentifierTypeRelative,
		Identifier:       logPath,
	}, -1)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	defer func() {
		_ = reader.Close()
	}()

	cd := mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(logPath)})
	w.Header().Set("Content-Disposition", cd)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, err = io.Copy(w, reader)
	if err != nil {
		c.Logger.WithError(err).WithField("log_path", logPath).Warn("Write run hook output")
	}
}

func (c *Controller) ListBranches(w http.ResponseWriter, r *http.Request, repository string, params apigen.ListBranchesParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListBranchesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_branches", r, repository, "", "")

	res, hasMore, err := c.Catalog.ListBranches(ctx, repository, paginationPrefix(params.Prefix), paginationAmount(params.Amount), paginationAfter(params.After))
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	refs := make([]apigen.Ref, 0, len(res))
	for _, branch := range res {
		refs = append(refs, apigen.Ref{
			CommitId: branch.Reference,
			Id:       branch.Name,
		})
	}
	response := apigen.RefList{
		Results:    refs,
		Pagination: paginationFor(hasMore, refs, "Id"),
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) CreateBranch(w http.ResponseWriter, r *http.Request, body apigen.CreateBranchJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateBranchAction,
			Resource: permissions.BranchArn(repository, body.Name),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_branch", r, repository, body.Name, "")
	commitLog, err := c.Catalog.CreateBranch(ctx, repository, body.Name, body.Source)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, commitLog.Reference)
}

func (c *Controller) DeleteBranch(w http.ResponseWriter, r *http.Request, repository, branch string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteBranchAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "delete_branch", r, repository, branch, "")
	err := c.Catalog.DeleteBranch(ctx, repository, branch)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetBranch(w http.ResponseWriter, r *http.Request, repository, branch string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadBranchAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_branch", r, repository, branch, "")
	reference, err := c.Catalog.GetBranchReference(ctx, repository, branch)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.Ref{
		CommitId: reference,
		Id:       branch,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) handleAPIErrorCallback(ctx context.Context, w http.ResponseWriter, r *http.Request, err error, cb func(w http.ResponseWriter, r *http.Request, code int, v interface{})) bool {
	// verify if request canceled even if there is no error, early exit point
	if httputil.IsRequestCanceled(r) {
		cb(w, r, httpStatusClientClosedRequest, httpStatusClientClosedRequestText)
		return true
	}
	if err == nil {
		return false
	}

	log := c.Logger.WithContext(ctx).WithError(err)

	// order of case is important, more specific errors should be first
	switch {
	case errors.Is(err, graveler.ErrAddressTokenNotFound),
		errors.Is(err, graveler.ErrAddressTokenExpired):
		log.Debug("Expired or invalid address token")
		cb(w, r, http.StatusBadRequest, "bad address token (expired or invalid)")

	case errors.Is(err, graveler.ErrNotFound),
		errors.Is(err, actions.ErrNotFound),
		errors.Is(err, auth.ErrNotFound),
		errors.Is(err, kv.ErrNotFound):
		log.Debug("Not found")
		cb(w, r, http.StatusNotFound, err)

	case errors.Is(err, block.ErrForbidden),
		errors.Is(err, graveler.ErrProtectedBranch):
		cb(w, r, http.StatusForbidden, err)

	case errors.Is(err, graveler.ErrDirtyBranch),
		errors.Is(err, graveler.ErrCommitMetaRangeDirtyBranch),
		errors.Is(err, graveler.ErrInvalidValue),
		errors.Is(err, validator.ErrInvalidValue),
		errors.Is(err, catalog.ErrPathRequiredValue),
		errors.Is(err, graveler.ErrNoChanges),
		errors.Is(err, permissions.ErrInvalidServiceName),
		errors.Is(err, permissions.ErrInvalidAction),
		errors.Is(err, model.ErrValidationError),
		errors.Is(err, graveler.ErrInvalidRef),
		errors.Is(err, actions.ErrParamConflict),
		errors.Is(err, graveler.ErrDereferenceCommitWithStaging),
		errors.Is(err, graveler.ErrParentOutOfRange),
		errors.Is(err, graveler.ErrCherryPickMergeNoParent),
		errors.Is(err, graveler.ErrInvalidMergeStrategy),
		errors.Is(err, block.ErrInvalidAddress),
		errors.Is(err, block.ErrOperationNotSupported):
		log.Debug("Bad request")
		cb(w, r, http.StatusBadRequest, err)

	case errors.Is(err, graveler.ErrNotUnique),
		errors.Is(err, graveler.ErrConflictFound),
		errors.Is(err, graveler.ErrRevertMergeNoParent):
		log.Debug("Conflict")
		cb(w, r, http.StatusConflict, err)

	case errors.Is(err, graveler.ErrLockNotAcquired):
		log.Debug("Lock not acquired")
		cb(w, r, http.StatusInternalServerError, "branch is currently locked, try again later")

	case errors.Is(err, block.ErrDataNotFound):
		log.Debug("No data")
		cb(w, r, http.StatusGone, "No data")

	case errors.Is(err, auth.ErrAlreadyExists):
		log.Debug("Already exists")
		cb(w, r, http.StatusConflict, "Already exists")

	case errors.Is(err, graveler.ErrTooManyTries):
		log.Debug("Retried too many times")
		cb(w, r, http.StatusLocked, "Too many attempts, try again later")

	case err != nil:
		c.Logger.WithContext(ctx).WithError(err).Error("API call returned status internal server error")
		cb(w, r, http.StatusInternalServerError, err)

	default:
		return false
	}

	return true
}

func (c *Controller) handleAPIError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) bool {
	return c.handleAPIErrorCallback(ctx, w, r, err, writeError)
}

func (c *Controller) ResetBranch(w http.ResponseWriter, r *http.Request, body apigen.ResetBranchJSONRequestBody, repository, branch string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.RevertBranchAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "reset_branch", r, repository, branch, "")

	var err error
	switch body.Type {
	case entryTypeCommonPrefix:
		err = c.Catalog.ResetEntries(ctx, repository, branch, apiutil.Value(body.Path))
	case "reset":
		err = c.Catalog.ResetBranch(ctx, repository, branch)
	case entryTypeObject:
		err = c.Catalog.ResetEntry(ctx, repository, branch, apiutil.Value(body.Path))
	default:
		writeError(w, r, http.StatusNotFound, "reset type not found")
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) ImportStart(w http.ResponseWriter, r *http.Request, body apigen.ImportStartJSONRequestBody, repository, branch string) {
	perm := permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.WriteObjectAction,
					Resource: permissions.BranchArn(repository, branch),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateBranchAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateCommitAction,
					Resource: permissions.BranchArn(repository, branch),
				},
			},
		},
	}
	// Add import permissions per source
	// Add object permissions per destination
	for _, source := range body.Paths {
		perm.Nodes = append(perm.Nodes,
			permissions.Node{Permission: permissions.Permission{
				Action:   permissions.ImportFromStorageAction,
				Resource: permissions.StorageNamespace(source.Path),
			}},
			permissions.Node{Permission: permissions.Permission{
				Action:   permissions.WriteObjectAction,
				Resource: permissions.ObjectArn(repository, source.Destination),
			}})
	}
	if !c.authorize(w, r, perm) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "import", r, repository, branch, "")
	user, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "missing user")
		return
	}
	metadata := map[string]string{}
	if body.Commit.Metadata != nil {
		metadata = body.Commit.Metadata.AdditionalProperties
	}
	paths := make([]catalog.ImportPath, 0, len(body.Paths))
	for _, p := range body.Paths {
		pathType, err := catalog.GetImportPathType(p.Type)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		paths = append(paths, catalog.ImportPath{
			Destination: p.Destination,
			Path:        p.Path,
			Type:        pathType,
		})
	}

	committer := user.Username
	importID, err := c.Catalog.Import(r.Context(), repository, branch, catalog.ImportRequest{
		Paths: paths,
		Commit: catalog.ImportCommit{
			CommitMessage: body.Commit.Message,
			Committer:     committer,
			Metadata:      metadata,
		},
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusAccepted, apigen.ImportCreationResponse{
		Id: importID,
	})
}

func importStatusToResponse(status *graveler.ImportStatus) apigen.ImportStatusResp {
	resp := apigen.ImportStatusResp{
		Completed:       status.Completed,
		IngestedObjects: &status.Progress,
		UpdateTime:      status.UpdatedAt,
	}

	if status.Error != nil {
		resp.Error = &apigen.Error{Message: status.Error.Error()}
	}
	if status.MetaRangeID != "" {
		metarange := status.MetaRangeID.String()
		resp.MetarangeId = &metarange
	}

	commitLog := catalog.CommitRecordToLog(status.Commit)
	if commitLog != nil {
		resp.Commit = &apigen.Commit{
			Committer:    commitLog.Committer,
			CreationDate: commitLog.CreationDate.Unix(),
			Id:           commitLog.Reference,
			Message:      commitLog.Message,
			MetaRangeId:  commitLog.MetaRangeID,
			Metadata:     &apigen.Commit_Metadata{AdditionalProperties: commitLog.Metadata},
			Parents:      commitLog.Parents,
		}
	}

	return resp
}

func (c *Controller) ImportStatus(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.ImportStatusParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadBranchAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	status, err := c.Catalog.GetImportStatus(ctx, repository, params.Id)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	resp := importStatusToResponse(status)
	writeResponse(w, r, http.StatusOK, resp)
}

func (c *Controller) ImportCancel(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.ImportCancelParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ImportCancelAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "cancel_import", r, repository, branch, "")
	err := c.Catalog.CancelImport(ctx, repository, params.Id)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) IngestRange(w http.ResponseWriter, r *http.Request, body apigen.IngestRangeJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ImportFromStorageAction,
					Resource: permissions.StorageNamespace(body.FromSourceURI),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.WriteObjectAction,
					Resource: permissions.ObjectArn(repository, body.Prepend),
				},
			},
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "ingest_range", r, repository, "", "")

	contToken := apiutil.Value(body.ContinuationToken)
	stagingToken := apiutil.Value(body.StagingToken)
	info, mark, err := c.Catalog.WriteRange(r.Context(), repository, catalog.WriteRangeRequest{
		SourceURI:         body.FromSourceURI,
		Prepend:           body.Prepend,
		After:             body.After,
		StagingToken:      stagingToken,
		ContinuationToken: contToken,
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	writeResponse(w, r, http.StatusCreated, apigen.IngestRangeCreationResponse{
		Range: &apigen.RangeMetadata{
			Id:            string(info.ID),
			MinKey:        string(info.MinKey),
			MaxKey:        string(info.MaxKey),
			Count:         info.Count,
			EstimatedSize: int(info.EstimatedRangeSizeBytes),
		},
		Pagination: &apigen.ImportPagination{
			HasMore:           mark.HasMore,
			ContinuationToken: &mark.ContinuationToken,
			LastKey:           mark.LastKey,
			StagingToken:      &mark.StagingToken,
		},
	})
}

func (c *Controller) CreateMetaRange(w http.ResponseWriter, r *http.Request, body apigen.CreateMetaRangeJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateMetaRangeAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "create_metarange", r, repository, "", "")

	ranges := make([]*graveler.RangeInfo, 0, len(body.Ranges))
	for _, r := range body.Ranges {
		ranges = append(ranges, &graveler.RangeInfo{
			ID:                      graveler.RangeID(r.Id),
			MinKey:                  graveler.Key(r.MinKey),
			MaxKey:                  graveler.Key(r.MaxKey),
			Count:                   r.Count,
			EstimatedRangeSizeBytes: uint64(r.EstimatedSize),
		})
	}
	info, err := c.Catalog.WriteMetaRange(r.Context(), repository, ranges)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusCreated, apigen.MetaRangeCreationResponse{
		Id: apiutil.Ptr(string(info.ID)),
	})
}

func (c *Controller) UpdateBranchToken(w http.ResponseWriter, r *http.Request, body apigen.UpdateBranchTokenJSONRequestBody, repository, branch string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action: permissions.WriteObjectAction,
			// This API writes an entire staging area to a branch and therefore requires permission to write to the entire repository space
			Resource: permissions.ObjectArn(repository, "*"),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "update_branch_token", r, repository, branch, "")
	err := c.Catalog.UpdateBranchToken(ctx, repository, branch, body.StagingToken)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) Commit(w http.ResponseWriter, r *http.Request, body apigen.CommitJSONRequestBody, repository, branch string, params apigen.CommitParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateCommitAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_commit", r, repository, branch, "")
	user, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "missing user")
		return
	}
	var metadata map[string]string
	if body.Metadata != nil {
		metadata = body.Metadata.AdditionalProperties
	}
	committer := user.Username
	newCommit, err := c.Catalog.Commit(ctx, repository, branch, body.Message, committer, metadata, body.Date, params.SourceMetarange)
	var hookAbortErr *graveler.HookAbortError
	if errors.As(err, &hookAbortErr) {
		c.Logger.
			WithError(err).
			WithField("run_id", hookAbortErr.RunID).
			Warn("aborted by hooks")
		writeError(w, r, http.StatusPreconditionFailed, err)
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	commitResponse(w, r, newCommit)
}

func commitResponse(w http.ResponseWriter, r *http.Request, newCommit *catalog.CommitLog) {
	response := apigen.Commit{
		Committer:    newCommit.Committer,
		CreationDate: newCommit.CreationDate.Unix(),
		Id:           newCommit.Reference,
		Message:      newCommit.Message,
		MetaRangeId:  newCommit.MetaRangeID,
		Metadata:     &apigen.Commit_Metadata{AdditionalProperties: newCommit.Metadata},
		Parents:      newCommit.Parents,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) DiffBranch(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.DiffBranchParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListObjectsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "diff_workspace", r, repository, branch, "")

	diff, hasMore, err := c.Catalog.DiffUncommitted(
		ctx,
		repository,
		branch,
		paginationPrefix(params.Prefix),
		paginationDelimiter(params.Delimiter),
		paginationAmount(params.Amount),
		paginationAfter(params.After),
	)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	results := make([]apigen.Diff, 0, len(diff))
	for _, d := range diff {
		pathType := entryTypeObject
		if d.CommonLevel {
			pathType = entryTypeCommonPrefix
		}
		diff := apigen.Diff{
			Path:     d.Path,
			Type:     transformDifferenceTypeToString(d.Type),
			PathType: pathType,
		}
		if !d.CommonLevel {
			diff.SizeBytes = &d.Size
		}
		results = append(results, diff)
	}
	response := apigen.DiffList{
		Pagination: paginationFor(hasMore, results, "Path"),
		Results:    results,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) DeleteObject(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.DeleteObjectParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "delete_object", r, repository, branch, "")

	err := c.Catalog.DeleteEntry(ctx, repository, branch, params.Path)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) UploadObjectPreflight(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.UploadObjectPreflightParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.WriteObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "put_object_preflight", r, repository, branch, "")

	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) UploadObject(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.UploadObjectParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.WriteObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "put_object", r, repository, branch, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// check if branch exists - it is still a possibility, but we don't want to upload large object when the branch was not there in the first place
	branchExists, err := c.Catalog.BranchExists(ctx, repository, branch)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	if !branchExists {
		writeError(w, r, http.StatusNotFound, fmt.Sprintf("branch '%s' not found", branch))
		return
	}

	// before writing body, ensure preconditions - this means we essentially check for object existence twice:
	// once before uploading the body to save resources and time,
	//	and then graveler will check again when passed a SetOptions.
	allowOverwrite := true
	if params.IfNoneMatch != nil {
		if apiutil.Value(params.IfNoneMatch) != "*" {
			writeError(w, r, http.StatusBadRequest, "Unsupported value for If-None-Match - Only \"*\" is supported")
			return
		}
		// check if exists
		_, err := c.Catalog.GetEntry(ctx, repo.Name, branch, params.Path, catalog.GetEntryParams{})
		if err == nil {
			writeError(w, r, http.StatusPreconditionFailed, "path already exists")
			return
		}
		if !errors.Is(err, graveler.ErrNotFound) {
			writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		allowOverwrite = false
	}

	// read request body parse multipart for "content" and upload the data
	mt, p, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if mt != "multipart/form-data" {
		writeError(w, r, http.StatusInternalServerError, http.ErrNotMultipart)
		return
	}
	boundary, ok := p["boundary"]
	if !ok {
		writeError(w, r, http.StatusInternalServerError, http.ErrMissingBoundary)
		return
	}

	reader := multipart.NewReader(r.Body, boundary)
	var (
		contentUploaded bool
		contentType     string
		blob            *upload.Blob
	)
	for !contentUploaded {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		contentType = part.Header.Get("Content-Type")
		partName := part.FormName()
		if partName == "content" {
			// upload the first "content" and exit the loop
			address := c.PathProvider.NewPath()
			blob, err = upload.WriteBlob(ctx, c.BlockAdapter, repo.StorageNamespace, address, part, -1, block.PutOpts{StorageClass: params.StorageClass})
			if err != nil {
				_ = part.Close()
				writeError(w, r, http.StatusInternalServerError, err)
				return
			}
			contentUploaded = true
		}
		_ = part.Close()
	}
	if !contentUploaded {
		err := fmt.Errorf("multipart upload missing key 'content': %w", http.ErrMissingFile)
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	// write metadata
	writeTime := time.Now()
	entryBuilder := catalog.NewDBEntryBuilder().
		Path(params.Path).
		PhysicalAddress(blob.PhysicalAddress).
		CreationDate(writeTime).
		Size(blob.Size).
		Checksum(blob.Checksum).
		ContentType(contentType)
	if blob.RelativePath {
		entryBuilder.AddressType(catalog.AddressTypeRelative)
	} else {
		entryBuilder.AddressType(catalog.AddressTypeFull)
	}
	meta := extractLakeFSMetadata(r.Header)
	if len(meta) > 0 {
		entryBuilder.Metadata(meta)
	}
	entry := entryBuilder.Build()

	err = c.Catalog.CreateEntry(ctx, repo.Name, branch, entry, graveler.WithIfAbsent(!allowOverwrite))
	if errors.Is(err, graveler.ErrPreconditionFailed) {
		writeError(w, r, http.StatusPreconditionFailed, "path already exists")
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	identifierType := block.IdentifierTypeFull
	if blob.RelativePath {
		identifierType = block.IdentifierTypeRelative
	}

	qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, blob.PhysicalAddress, identifierType)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	response := apigen.ObjectStats{
		Checksum:        blob.Checksum,
		Mtime:           writeTime.Unix(),
		Path:            params.Path,
		PathType:        entryTypeObject,
		PhysicalAddress: qk.Format(),
		SizeBytes:       apiutil.Ptr(blob.Size),
		ContentType:     &contentType,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) StageObject(w http.ResponseWriter, r *http.Request, body apigen.StageObjectJSONRequestBody, repository, branch string, params apigen.StageObjectParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.WriteObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "stage_object", r, repository, branch, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	// write metadata
	qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, body.PhysicalAddress, block.IdentifierTypeFull)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// see what storage type this is and whether it fits our configuration
	uriRegex := c.BlockAdapter.GetStorageNamespaceInfo().ValidityRegex
	if match, err := regexp.MatchString(uriRegex, body.PhysicalAddress); err != nil || !match {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("physical address is not valid for block adapter: %s",
			c.BlockAdapter.BlockstoreType(),
		))
		return
	}

	// take mtime from request, if any
	writeTime := time.Now()
	if body.Mtime != nil {
		writeTime = time.Unix(*body.Mtime, 0)
	}

	physicalAddress, addressType := normalizePhysicalAddress(repo.StorageNamespace, body.PhysicalAddress)

	entryBuilder := catalog.NewDBEntryBuilder().
		CommonLevel(false).
		Path(params.Path).
		PhysicalAddress(physicalAddress).
		AddressType(addressType).
		CreationDate(writeTime).
		Size(body.SizeBytes).
		Checksum(body.Checksum).
		ContentType(apiutil.Value(body.ContentType))
	if body.Metadata != nil {
		entryBuilder.Metadata(body.Metadata.AdditionalProperties)
	}
	entry := entryBuilder.Build()

	err = c.Catalog.CreateEntry(ctx, repo.Name, branch, entry)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.ObjectStats{
		Checksum:        entry.Checksum,
		Mtime:           entry.CreationDate.Unix(),
		Path:            entry.Path,
		PathType:        entryTypeObject,
		PhysicalAddress: qk.Format(),
		SizeBytes:       apiutil.Ptr(entry.Size),
		ContentType:     &entry.ContentType,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) CopyObject(w http.ResponseWriter, r *http.Request, body apigen.CopyObjectJSONRequestBody, repository, branch string, params apigen.CopyObjectParams) {
	srcPath := body.SrcPath
	destPath := params.DestPath
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadActionsAction,
					Resource: permissions.ObjectArn(repository, srcPath),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.WriteObjectAction,
					Resource: permissions.ObjectArn(repository, destPath),
				},
			},
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "copy_object", r, repository, branch, destPath)

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// verify destination is a branch (and exists)
	branchExists, err := c.Catalog.BranchExists(ctx, repository, branch)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	if !branchExists {
		writeError(w, r, http.StatusNotFound, fmt.Sprintf("branch '%s' not found", branch))
		return
	}

	// use destination branch as source if not specified
	srcRef := swag.StringValue(body.SrcRef)
	if srcRef == "" {
		srcRef = branch
	}

	// copy entry
	entry, err := c.Catalog.CopyEntry(ctx, repository, srcRef, srcPath, repository, branch, destPath)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, entry.PhysicalAddress, block.IdentifierTypeRelative)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	response := apigen.ObjectStats{
		Checksum:        entry.Checksum,
		Mtime:           entry.CreationDate.Unix(),
		Path:            entry.Path,
		PathType:        entryTypeObject,
		PhysicalAddress: qk.Format(),
		SizeBytes:       apiutil.Ptr(entry.Size),
		ContentType:     apiutil.Ptr(entry.ContentType),
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) RevertBranch(w http.ResponseWriter, r *http.Request, body apigen.RevertBranchJSONRequestBody, repository, branch string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.RevertBranchAction,
			Resource: permissions.BranchArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "revert_branch", r, repository, branch, "")
	user, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "user not found")
		return
	}
	committer := user.Username
	err = c.Catalog.Revert(ctx, repository, branch, catalog.RevertParams{
		Reference:    body.Ref,
		Committer:    committer,
		ParentNumber: body.ParentNumber,
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) CherryPick(w http.ResponseWriter, r *http.Request, body apigen.CherryPickJSONRequestBody, repository string, branch string) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateCommitAction,
					Resource: permissions.BranchArn(repository, branch),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadCommitAction,
					Resource: permissions.RepoArn(repository),
				},
			},
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "cherry_pick", r, repository, branch, body.Ref)
	user, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "user not found")
		return
	}
	committer := user.Username
	newCommit, err := c.Catalog.CherryPick(ctx, repository, branch, catalog.CherryPickParams{
		Reference:    body.Ref,
		Committer:    committer,
		ParentNumber: body.ParentNumber,
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	commitResponse(w, r, newCommit)
}

func (c *Controller) GetCommit(w http.ResponseWriter, r *http.Request, repository, commitID string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadCommitAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_commit", r, repository, commitID, "")
	commit, err := c.Catalog.GetCommit(ctx, repository, commitID)
	if errors.Is(err, graveler.ErrRepositoryNotFound) {
		writeError(w, r, http.StatusNotFound, "repository not found")
		return
	}
	if errors.Is(err, graveler.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "commit not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	response := apigen.Commit{
		Id:           commit.Reference,
		Committer:    commit.Committer,
		CreationDate: commit.CreationDate.Unix(),
		Message:      commit.Message,
		MetaRangeId:  commit.MetaRangeID,
		Metadata:     &apigen.Commit_Metadata{AdditionalProperties: commit.Metadata},
		Parents:      commit.Parents,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) GetGarbageCollectionRules(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.GetGarbageCollectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	rules, err := c.Catalog.GetGarbageCollectionRules(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	resp := apigen.GarbageCollectionRules{}
	resp.DefaultRetentionDays = int(rules.DefaultRetentionDays)
	for branchID, retentionDays := range rules.BranchRetentionDays {
		resp.Branches = append(resp.Branches, apigen.GarbageCollectionRule{BranchId: branchID, RetentionDays: int(retentionDays)})
	}
	writeResponse(w, r, http.StatusOK, resp)
}

func (c *Controller) SetGarbageCollectionRulesPreflight(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.SetGarbageCollectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "set_gc_collection_rules_preflight", r, repository, "", "")

	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) SetGarbageCollectionRules(w http.ResponseWriter, r *http.Request, body apigen.SetGarbageCollectionRulesJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.SetGarbageCollectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	rules := &graveler.GarbageCollectionRules{
		DefaultRetentionDays: int32(body.DefaultRetentionDays),
		BranchRetentionDays:  make(map[string]int32),
	}
	for _, rule := range body.Branches {
		rules.BranchRetentionDays[rule.BranchId] = int32(rule.RetentionDays)
	}
	err := c.Catalog.SetGarbageCollectionRules(ctx, repository, rules)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) DeleteGarbageCollectionRules(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.SetGarbageCollectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	err := c.Catalog.SetGarbageCollectionRules(ctx, repository, nil)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) PrepareGarbageCollectionCommits(w http.ResponseWriter, r *http.Request, body apigen.PrepareGarbageCollectionCommitsJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.PrepareGarbageCollectionCommitsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "prepare_garbage_collection_commits", r, repository, "", "")
	gcRunMetadata, err := c.Catalog.PrepareExpiredCommits(ctx, repository, apiutil.Value(body.PreviousRunId))
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	presignedURL, _, err := c.BlockAdapter.GetPreSignedURL(ctx, block.ObjectPointer{
		Identifier:     gcRunMetadata.CommitsCSVLocation,
		IdentifierType: block.IdentifierTypeFull,
	}, block.PreSignModeRead)
	if err != nil {
		c.Logger.WithError(err).Warn("Failed to presign url for GC commits")
	}
	writeResponse(w, r, http.StatusCreated, apigen.GarbageCollectionPrepareResponse{
		GcCommitsLocation:     gcRunMetadata.CommitsCSVLocation,
		GcAddressesLocation:   gcRunMetadata.AddressLocation,
		RunId:                 gcRunMetadata.RunID,
		GcCommitsPresignedUrl: &presignedURL,
	})
}

func (c *Controller) GetBranchProtectionRules(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.GetBranchProtectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	rules, err := c.Catalog.GetBranchProtectionRules(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	resp := make([]*apigen.BranchProtectionRule, 0, len(rules.BranchPatternToBlockedActions))
	for pattern := range rules.BranchPatternToBlockedActions {
		resp = append(resp, &apigen.BranchProtectionRule{
			Pattern: pattern,
		})
	}
	writeResponse(w, r, http.StatusOK, resp)
}

func (c *Controller) DeleteBranchProtectionRule(w http.ResponseWriter, r *http.Request, body apigen.DeleteBranchProtectionRuleJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.SetBranchProtectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "delete_branch_protection_rule", r, repository, "", "")

	err := c.Catalog.DeleteBranchProtectionRule(ctx, repository, body.Pattern)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) CreateBranchProtectionRulePreflight(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.SetBranchProtectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}

	ctx := r.Context()
	c.LogAction(ctx, "create_branch_protection_rule_preflight", r, repository, "", "")

	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) CreateBranchProtectionRule(w http.ResponseWriter, r *http.Request, body apigen.CreateBranchProtectionRuleJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.SetBranchProtectionRulesAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_branch_protection_rule", r, repository, "", "")

	// For now, all protected branches use the same default set of blocked actions. In the future this set will be user configurable.
	blockedActions := []graveler.BranchProtectionBlockedAction{graveler.BranchProtectionBlockedAction_STAGING_WRITE, graveler.BranchProtectionBlockedAction_COMMIT}
	err := c.Catalog.CreateBranchProtectionRule(ctx, repository, body.Pattern, blockedActions)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetMetaRange(w http.ResponseWriter, r *http.Request, repository, metaRange string) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ListObjectsAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadRepositoryAction,
					Resource: permissions.RepoArn(repository),
				},
			},
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "metadata_get_metarange", r, repository, "", "")

	metarange, err := c.Catalog.GetMetaRange(ctx, repository, metaRange)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.StorageURI{
		Location: string(metarange),
	}
	w.Header().Set("Location", string(metarange))
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) GetRange(w http.ResponseWriter, r *http.Request, repository, pRange string) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ListObjectsAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.ReadRepositoryAction,
					Resource: permissions.RepoArn(repository),
				},
			},
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "metadata_get_range", r, repository, "", "")

	rng, err := c.Catalog.GetRange(ctx, repository, pRange)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.StorageURI{
		Location: string(rng),
	}
	w.Header().Set("Location", string(rng))
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) DumpRefs(w http.ResponseWriter, r *http.Request, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.ListTagsAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.ListBranchesAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.ListCommitsAction,
					Resource: permissions.RepoArn(repository),
				},
			},
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "dump_repository_refs", r, repository, "", "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// dump all types:
	tagsID, err := c.Catalog.DumpTags(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	branchesID, err := c.Catalog.DumpBranches(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	commitsID, err := c.Catalog.DumpCommits(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	response := apigen.RefsDump{
		BranchesMetaRangeId: branchesID,
		CommitsMetaRangeId:  commitsID,
		TagsMetaRangeId:     tagsID,
	}

	// write this to the block store
	manifestBytes, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	err = c.BlockAdapter.Put(ctx, block.ObjectPointer{
		StorageNamespace: repo.StorageNamespace,
		IdentifierType:   block.IdentifierTypeRelative,
		Identifier:       fmt.Sprintf("%s/refs_manifest.json", c.Config.Committed.BlockStoragePrefix),
	}, int64(len(manifestBytes)), bytes.NewReader(manifestBytes), block.PutOpts{})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) RestoreRefs(w http.ResponseWriter, r *http.Request, body apigen.RestoreRefsJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Type: permissions.NodeTypeAnd,
		Nodes: []permissions.Node{
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateTagAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateBranchAction,
					Resource: permissions.RepoArn(repository),
				},
			},
			{
				Permission: permissions.Permission{
					Action:   permissions.CreateCommitAction,
					Resource: permissions.RepoArn(repository),
				},
			},
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "restore_repository_refs", r, repository, "", "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// ensure no refs currently found
	_, _, err = c.Catalog.ListCommits(ctx, repo.Name, repo.DefaultBranch, catalog.LogParams{
		PathList:      make([]catalog.PathRecord, 0),
		FromReference: "",
		Amount:        1,
	})
	if !errors.Is(err, graveler.ErrNotFound) {
		writeError(w, r, http.StatusBadRequest, "can only restore into a bare repository")
		return
	}

	// load commits
	err = c.Catalog.LoadCommits(ctx, repo.Name, body.CommitsMetaRangeId)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	err = c.Catalog.LoadBranches(ctx, repo.Name, body.BranchesMetaRangeId)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	err = c.Catalog.LoadTags(ctx, repo.Name, body.TagsMetaRangeId)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
}

func (c *Controller) CreateSymlinkFile(w http.ResponseWriter, r *http.Request, repository, branch string, params apigen.CreateSymlinkFileParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.WriteObjectAction,
			Resource: permissions.ObjectArn(repository, branch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_symlink", r, repository, branch, "")

	// read repo
	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// list entries
	var currentPath string
	var currentAddresses []string
	var after string
	var entries []*catalog.DBEntry
	hasMore := true
	for hasMore {
		entries, hasMore, err = c.Catalog.ListEntries(
			ctx,
			repository,
			branch,
			apiutil.Value(params.Location),
			after,
			"",
			-1)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		// loop all entries enter to map[path] physicalAddress
		for _, entry := range entries {
			qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, entry.PhysicalAddress, entry.AddressType.ToIdentifierType())
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, fmt.Sprintf("error while resolving address: %s", err))
				return
			}
			idx := strings.LastIndex(entry.Path, "/")
			var path string
			if idx != -1 {
				path = entry.Path[0:idx]
			}
			if path != currentPath {
				// push current
				err := writeSymlink(ctx, repo, branch, path, currentAddresses, c.BlockAdapter)
				if err != nil {
					writeError(w, r, http.StatusInternalServerError, fmt.Sprintf("error while writing symlinks: %s", err))
					return
				}
				currentPath = path
				currentAddresses = []string{qk.Format()}
			} else {
				currentAddresses = append(currentAddresses, qk.Format())
			}
		}
		after = entries[len(entries)-1].Path
	}
	if len(currentAddresses) > 0 {
		err = writeSymlink(ctx, repo, branch, currentPath, currentAddresses, c.BlockAdapter)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, fmt.Sprintf("error while writing symlinks: %s", err))
			return
		}
	}
	metaLocation := fmt.Sprintf("%s/%s", repo.StorageNamespace, lakeFSPrefix)
	response := apigen.StorageURI{
		Location: metaLocation,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func writeSymlink(ctx context.Context, repo *catalog.Repository, branch, path string, addresses []string, adapter block.Adapter) error {
	address := fmt.Sprintf("%s/%s/%s/%s/symlink.txt", lakeFSPrefix, repo.Name, branch, path)
	data := strings.Join(addresses, "\n")
	symlinkReader := aws.ReadSeekCloser(strings.NewReader(data))
	err := adapter.Put(ctx, block.ObjectPointer{
		StorageNamespace: repo.StorageNamespace,
		IdentifierType:   block.IdentifierTypeRelative,
		Identifier:       address,
	}, int64(len(data)), symlinkReader, block.PutOpts{})
	return err
}

func (c *Controller) DiffRefs(w http.ResponseWriter, r *http.Request, repository, leftRef, rightRef string, params apigen.DiffRefsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListObjectsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "diff_refs", r, repository, rightRef, leftRef)
	diffFunc := c.Catalog.Compare // default diff type is three-dot
	if params.Type != nil && *params.Type == "two_dot" {
		diffFunc = c.Catalog.Diff
	}

	diff, hasMore, err := diffFunc(ctx, repository, leftRef, rightRef, catalog.DiffParams{
		Limit:            paginationAmount(params.Amount),
		After:            paginationAfter(params.After),
		Prefix:           paginationPrefix(params.Prefix),
		Delimiter:        paginationDelimiter(params.Delimiter),
		AdditionalFields: nil,
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	results := make([]apigen.Diff, 0, len(diff))
	for _, d := range diff {
		pathType := entryTypeObject
		if d.CommonLevel {
			pathType = entryTypeCommonPrefix
		}
		diff := apigen.Diff{
			Path:     d.Path,
			Type:     transformDifferenceTypeToString(d.Type),
			PathType: pathType,
		}
		if !d.CommonLevel {
			diff.SizeBytes = &d.Size
		}
		results = append(results, diff)
	}
	response := apigen.DiffList{
		Pagination: paginationFor(hasMore, results, "Path"),
		Results:    results,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) LogCommits(w http.ResponseWriter, r *http.Request, repository, ref string, params apigen.LogCommitsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadBranchAction,
			Resource: permissions.BranchArn(repository, ref),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_branch_commit_log", r, repository, ref, "")

	// get commit log
	commitLog, hasMore, err := c.Catalog.ListCommits(ctx, repository, ref, catalog.LogParams{
		PathList:      resolvePathList(params.Objects, params.Prefixes),
		FromReference: paginationAfter(params.After),
		Amount:        paginationAmount(params.Amount),
		Limit:         swag.BoolValue(params.Limit),
		FirstParent:   swag.BoolValue(params.FirstParent),
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	serializedCommits := make([]apigen.Commit, 0, len(commitLog))
	for _, commit := range commitLog {
		metadata := apigen.Commit_Metadata{
			AdditionalProperties: commit.Metadata,
		}
		serializedCommits = append(serializedCommits, apigen.Commit{
			Committer:    commit.Committer,
			CreationDate: commit.CreationDate.Unix(),
			Id:           commit.Reference,
			Message:      commit.Message,
			Metadata:     &metadata,
			MetaRangeId:  commit.MetaRangeID,
			Parents:      commit.Parents,
		})
	}

	response := apigen.CommitList{
		Pagination: paginationFor(hasMore, serializedCommits, "Id"),
		Results:    serializedCommits,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) HeadObject(w http.ResponseWriter, r *http.Request, repository, ref string, params apigen.HeadObjectParams) {
	if !c.authorizeCallback(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}, func(w http.ResponseWriter, r *http.Request, code int, v interface{}) {
		writeResponse(w, r, code, nil)
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "head_object", r, repository, ref, "")

	// read the FS entry
	entry, err := c.Catalog.GetEntry(ctx, repository, ref, params.Path, catalog.GetEntryParams{})
	if err != nil {
		c.handleAPIErrorCallback(ctx, w, r, err, func(w http.ResponseWriter, r *http.Request, code int, v interface{}) {
			writeResponse(w, r, code, nil)
		})
		return
	}
	if entry.Expired {
		writeResponse(w, r, http.StatusGone, nil)
		return
	}

	etag := httputil.ETag(entry.Checksum)
	w.Header().Set("ETag", etag)
	lastModified := httputil.HeaderTimestamp(entry.CreationDate)
	w.Header().Set("Last-Modified", lastModified)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", entry.ContentType)
	// for security, make sure the browser and any proxies en route don't cache the response
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Expires", "0")

	// calculate possible byte range, if any.
	if params.Range != nil {
		rng, err := httputil.ParseRange(*params.Range, entry.Size)
		if err != nil {
			writeResponse(w, r, http.StatusRequestedRangeNotSatisfiable, nil)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.StartOffset, rng.EndOffset, entry.Size))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rng.EndOffset-rng.StartOffset+1))
		writeResponse(w, r, http.StatusPartialContent, nil)
	} else {
		w.Header().Set("Content-Length", fmt.Sprint(entry.Size))
	}
}

func (c *Controller) GetObject(w http.ResponseWriter, r *http.Request, repository, ref string, params apigen.GetObjectParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_object", r, repository, ref, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// read the FS entry
	entry, err := c.Catalog.GetEntry(ctx, repository, ref, params.Path, catalog.GetEntryParams{})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	c.Logger.Tracef("get repo %s ref %s path %s: %+v", repository, ref, params.Path, entry)
	if entry.Expired {
		writeError(w, r, http.StatusGone, "resource expired")
		return
	}

	// if pre-sign, return a redirect
	pointer := block.ObjectPointer{
		StorageNamespace: repo.StorageNamespace,
		IdentifierType:   entry.AddressType.ToIdentifierType(),
		Identifier:       entry.PhysicalAddress,
	}
	if swag.BoolValue(params.Presign) {
		location, _, err := c.BlockAdapter.GetPreSignedURL(ctx, pointer, block.PreSignModeRead)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
		return
	}

	// setup response
	var reader io.ReadCloser

	// handle partial response if byte range supplied
	if params.Range != nil {
		rng, err := httputil.ParseRange(*params.Range, entry.Size)
		if err != nil {
			writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "Requested Range Not Satisfiable")
			return
		}
		reader, err = c.BlockAdapter.GetRange(ctx, pointer, rng.StartOffset, rng.EndOffset)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		defer func() {
			_ = reader.Close()
		}()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.StartOffset, rng.EndOffset, entry.Size))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rng.EndOffset-rng.StartOffset+1))
		writeResponse(w, r, http.StatusPartialContent, nil)
	} else {
		reader, err = c.BlockAdapter.Get(ctx, pointer, entry.Size)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		defer func() {
			_ = reader.Close()
		}()
		w.Header().Set("Content-Length", fmt.Sprint(entry.Size))
	}

	etag := httputil.ETag(entry.Checksum)
	w.Header().Set("ETag", etag)
	lastModified := httputil.HeaderTimestamp(entry.CreationDate)
	w.Header().Set("Last-Modified", lastModified)
	w.Header().Set("Content-Type", entry.ContentType)
	// for security, make sure the browser and any proxies en route don't cache the response
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	_, err = io.Copy(w, reader)
	if err != nil {
		c.Logger.
			WithError(err).
			WithFields(logging.Fields{
				"storage_namespace": repo.StorageNamespace,
				"physical_address":  entry.PhysicalAddress,
			}).
			Debug("GetObject copy content")
	}
}

func (c *Controller) ListObjects(w http.ResponseWriter, r *http.Request, repository, ref string, params apigen.ListObjectsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListObjectsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	user, _ := auth.GetUser(ctx)
	c.LogAction(ctx, "list_objects", r, repository, ref, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	res, hasMore, err := c.Catalog.ListEntries(
		ctx,
		repository,
		ref,
		paginationPrefix(params.Prefix),
		paginationAfter(params.After),
		paginationDelimiter(params.Delimiter),
		paginationAmount(params.Amount),
	)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	objList := make([]apigen.ObjectStats, 0, len(res))
	for _, entry := range res {
		qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, entry.PhysicalAddress, entry.AddressType.ToIdentifierType())
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, err)
			return
		}

		if entry.CommonLevel {
			objList = append(objList, apigen.ObjectStats{
				Path:     entry.Path,
				PathType: entryTypeCommonPrefix,
			})
		} else {
			var mtime int64
			if !entry.CreationDate.IsZero() {
				mtime = entry.CreationDate.Unix()
			}
			objStat := apigen.ObjectStats{
				Checksum:        entry.Checksum,
				Mtime:           mtime,
				Path:            entry.Path,
				PhysicalAddress: qk.Format(),
				PathType:        entryTypeObject,
				SizeBytes:       apiutil.Ptr(entry.Size),
				ContentType:     &entry.ContentType,
			}
			if (params.UserMetadata == nil || *params.UserMetadata) && entry.Metadata != nil {
				objStat.Metadata = &apigen.ObjectUserMetadata{AdditionalProperties: entry.Metadata}
			}
			if swag.BoolValue(params.Presign) {
				// check if the user has read permissions for this object
				authResponse, err := c.Auth.Authorize(ctx, &auth.AuthorizationRequest{
					Username: user.Username,
					RequiredPermissions: permissions.Node{
						Permission: permissions.Permission{
							Action:   permissions.ReadObjectAction,
							Resource: permissions.ObjectArn(repository, entry.Path),
						},
					},
				})
				if c.handleAPIError(ctx, w, r, err) {
					return
				}
				if authResponse.Allowed {
					var expiry time.Time
					objStat.PhysicalAddress, expiry, err = c.BlockAdapter.GetPreSignedURL(ctx, block.ObjectPointer{
						StorageNamespace: repo.StorageNamespace,
						IdentifierType:   entry.AddressType.ToIdentifierType(),
						Identifier:       entry.PhysicalAddress,
					}, block.PreSignModeRead)
					if c.handleAPIError(ctx, w, r, err) {
						return
					}
					if !expiry.IsZero() {
						objStat.PhysicalAddressExpiry = apiutil.Ptr(expiry.Unix())
					}
				}
			}
			objList = append(objList, objStat)
		}
	}
	response := apigen.ObjectStatsList{
		Pagination: apigen.Pagination{
			HasMore:    hasMore,
			MaxPerPage: DefaultMaxPerPage,
			Results:    len(objList),
		},
		Results: objList,
	}
	if len(objList) > 0 && hasMore {
		lastObj := objList[len(objList)-1]
		response.Pagination.NextOffset = lastObj.Path
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) StatObject(w http.ResponseWriter, r *http.Request, repository, ref string, params apigen.StatObjectParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "stat_object", r, repository, ref, "")

	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	entry, err := c.Catalog.GetEntry(ctx, repository, ref, params.Path, catalog.GetEntryParams{})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	qk, err := c.BlockAdapter.ResolveNamespace(repo.StorageNamespace, entry.PhysicalAddress, entry.AddressType.ToIdentifierType())
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	objStat := apigen.ObjectStats{
		Checksum:        entry.Checksum,
		Mtime:           entry.CreationDate.Unix(),
		Path:            entry.Path,
		PathType:        entryTypeObject,
		PhysicalAddress: qk.Format(),
		SizeBytes:       apiutil.Ptr(entry.Size),
		ContentType:     &entry.ContentType,
	}
	if (params.UserMetadata == nil || *params.UserMetadata) && entry.Metadata != nil {
		objStat.Metadata = &apigen.ObjectUserMetadata{AdditionalProperties: entry.Metadata}
	}
	code := http.StatusOK
	if entry.Expired {
		code = http.StatusGone
	} else if swag.BoolValue(params.Presign) {
		// need to pre-sign the physical address
		preSignedURL, expiry, err := c.BlockAdapter.GetPreSignedURL(ctx, block.ObjectPointer{
			StorageNamespace: repo.StorageNamespace,
			IdentifierType:   entry.AddressType.ToIdentifierType(),
			Identifier:       entry.PhysicalAddress,
		}, block.PreSignModeRead)
		if c.handleAPIError(ctx, w, r, err) {
			return
		}
		objStat.PhysicalAddress = preSignedURL
		if !expiry.IsZero() {
			objStat.PhysicalAddressExpiry = apiutil.Ptr(expiry.Unix())
		}
	}
	writeResponse(w, r, code, objStat)
}

func (c *Controller) GetUnderlyingProperties(w http.ResponseWriter, r *http.Request, repository, ref string, params apigen.GetUnderlyingPropertiesParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadObjectAction,
			Resource: permissions.ObjectArn(repository, params.Path),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "object_underlying_properties", r, repository, ref, "")

	// read repo
	repo, err := c.Catalog.GetRepository(ctx, repository)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	entry, err := c.Catalog.GetEntry(ctx, repository, ref, params.Path, catalog.GetEntryParams{})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	// read object properties from underlying storage
	properties, err := c.BlockAdapter.GetProperties(ctx, block.ObjectPointer{
		StorageNamespace: repo.StorageNamespace,
		IdentifierType:   entry.AddressType.ToIdentifierType(),
		Identifier:       entry.PhysicalAddress,
	})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	response := apigen.UnderlyingObjectProperties{
		StorageClass: properties.StorageClass,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) MergeIntoBranch(w http.ResponseWriter, r *http.Request, body apigen.MergeIntoBranchJSONRequestBody, repository, sourceRef, destinationBranch string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateCommitAction,
			Resource: permissions.BranchArn(repository, destinationBranch),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "merge_branches", r, repository, destinationBranch, sourceRef)
	user, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "user not found")
		return
	}
	metadata := map[string]string{}
	if body.Metadata != nil {
		metadata = body.Metadata.AdditionalProperties
	}

	reference, err := c.Catalog.Merge(ctx,
		repository, destinationBranch, sourceRef,
		user.Username,
		apiutil.Value(body.Message),
		metadata,
		apiutil.Value(body.Strategy))

	var hookAbortErr *graveler.HookAbortError
	switch {
	case errors.As(err, &hookAbortErr):
		c.Logger.WithError(err).WithField("run_id", hookAbortErr.RunID).Warn("aborted by hooks")
		writeError(w, r, http.StatusPreconditionFailed, err)
		return
	case errors.Is(err, graveler.ErrConflictFound):
		writeResponse(w, r, http.StatusConflict, apigen.MergeResult{
			Reference: reference,
		})
		return
	}
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusOK, apigen.MergeResult{
		Reference: reference,
	})
}

func (c *Controller) FindMergeBase(w http.ResponseWriter, r *http.Request, repository string, sourceRef string, destinationRef string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListCommitsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "find_merge_base", r, repository, destinationRef, sourceRef)

	source, dest, base, err := c.Catalog.FindMergeBase(ctx, repository, destinationRef, sourceRef)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusOK, apigen.FindMergeBaseResult{
		BaseCommitId:        base,
		DestinationCommitId: dest,
		SourceCommitId:      source,
	})
}

func (c *Controller) ListTags(w http.ResponseWriter, r *http.Request, repository string, params apigen.ListTagsParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ListTagsAction,
			Resource: permissions.RepoArn(repository),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "list_tags", r, repository, "", "")

	res, hasMore, err := c.Catalog.ListTags(ctx, repository, paginationPrefix(params.Prefix), paginationAmount(params.Amount), paginationAfter(params.After))
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	results := make([]apigen.Ref, 0, len(res))
	for _, tag := range res {
		results = append(results, apigen.Ref{
			CommitId: tag.CommitID,
			Id:       tag.ID,
		})
	}
	response := apigen.RefList{
		Results:    results,
		Pagination: paginationFor(hasMore, results, "Id"),
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) CreateTag(w http.ResponseWriter, r *http.Request, body apigen.CreateTagJSONRequestBody, repository string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.CreateTagAction,
			Resource: permissions.TagArn(repository, body.Id),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "create_tag", r, repository, body.Id, "")

	commitID, err := c.Catalog.CreateTag(ctx, repository, body.Id, body.Ref)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.Ref{
		CommitId: commitID,
		Id:       body.Id,
	}
	writeResponse(w, r, http.StatusCreated, response)
}

func (c *Controller) DeleteTag(w http.ResponseWriter, r *http.Request, repository, tag string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.DeleteTagAction,
			Resource: permissions.TagArn(repository, tag),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "delete_tag", r, repository, tag, "")
	err := c.Catalog.DeleteTag(ctx, repository, tag)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) GetTag(w http.ResponseWriter, r *http.Request, repository, tag string) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadTagAction,
			Resource: permissions.TagArn(repository, tag),
		},
	}) {
		return
	}
	ctx := r.Context()
	c.LogAction(ctx, "get_tag", r, repository, tag, "")
	reference, err := c.Catalog.GetTag(ctx, repository, tag)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	response := apigen.Ref{
		CommitId: reference,
		Id:       tag,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func newLoginConfig(c *config.Config) *apigen.LoginConfig {
	return &apigen.LoginConfig{
		RBAC:               &c.Auth.UIConfig.RBAC,
		LoginUrl:           c.Auth.UIConfig.LoginURL,
		LoginFailedMessage: &c.Auth.UIConfig.LoginFailedMessage,
		FallbackLoginUrl:   c.Auth.UIConfig.FallbackLoginURL,
		FallbackLoginLabel: c.Auth.UIConfig.FallbackLoginLabel,
		LoginCookieNames:   c.Auth.UIConfig.LoginCookieNames,
		LogoutUrl:          c.Auth.UIConfig.LogoutURL,
	}
}

func (c *Controller) GetSetupState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// external auth reports as initialized to avoid triggering the setup wizard
	if c.Config.Auth.UIConfig.RBAC == config.AuthRBACExternal {
		response := apigen.SetupState{
			State:            swag.String(string(auth.SetupStateInitialized)),
			LoginConfig:      newLoginConfig(c.Config),
			CommPrefsMissing: swag.Bool(false),
		}
		writeResponse(w, r, http.StatusOK, response)
		return
	}

	savedState, err := c.MetadataManager.GetSetupState(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if savedState == auth.SetupStateNotInitialized {
		c.Collector.CollectEvent(stats.Event{Class: "global", Name: "preinit", Client: httputil.GetRequestLakeFSClient(r)})
	}

	response := apigen.SetupState{
		State:       swag.String(string(savedState)),
		LoginConfig: newLoginConfig(c.Config),
	}

	// if email subscription is disabled in the config, set missing flag to false.
	// otherwise, check if the comm prefs are set. if they are, set missing flag to false.
	if !c.Config.EmailSubscription.Enabled {
		response.CommPrefsMissing = swag.Bool(false)
		writeResponse(w, r, http.StatusOK, response)
		return
	}

	prefsSet, err := c.MetadataManager.IsCommPrefsSet(ctx)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		// comprefs may not be found for two reasons:
		// 1. The setup ran on an older version of lakeFS that didn't have commprefs. In this case, we treat it as set.
		// 2. The setup ran on a newer version of lakeFS that has commprefs, but the setup didn't complete. In this case, we treat it as not set.
		response.CommPrefsMissing = swag.Bool(savedState != auth.SetupStateInitialized)
	case err != nil:
		// failed to check if comm prefs are set, treating as set
		response.CommPrefsMissing = swag.Bool(false)
	default:
		response.CommPrefsMissing = swag.Bool(!prefsSet)
	}

	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) Setup(w http.ResponseWriter, r *http.Request, body apigen.SetupJSONRequestBody) {
	if len(body.Username) == 0 {
		writeError(w, r, http.StatusBadRequest, "empty user display name")
		return
	}

	ctx := r.Context()
	initialized, err := c.MetadataManager.IsInitialized(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if initialized {
		writeError(w, r, http.StatusConflict, "lakeFS already initialized")
		return
	}

	// migrate the database if needed
	err = c.Migrator.Migrate(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	if c.Config.Auth.UIConfig.RBAC == config.AuthRBACExternal {
		// nothing to do - users are managed elsewhere
		writeResponse(w, r, http.StatusOK, apigen.CredentialsWithSecret{})
		return
	}

	var cred *model.Credential
	if body.Key == nil {
		cred, err = setup.CreateInitialAdminUser(ctx, c.Auth, c.Config, c.MetadataManager, body.Username)
	} else {
		cred, err = setup.CreateInitialAdminUserWithKeys(ctx, c.Auth, c.Config, c.MetadataManager, body.Username, &body.Key.AccessKeyId, &body.Key.SecretAccessKey)
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	meta := stats.NewMetadata(ctx, c.Logger, c.BlockAdapter.BlockstoreType(), c.MetadataManager, c.CloudMetadataProvider)
	c.Collector.SetInstallationID(meta.InstallationID)
	c.Collector.CollectMetadata(meta)
	c.Collector.CollectEvent(stats.Event{Class: "global", Name: "init", UserID: body.Username, Client: httputil.GetRequestLakeFSClient(r)})

	response := apigen.CredentialsWithSecret{
		AccessKeyId:     cred.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		CreationDate:    cred.IssuedDate.Unix(),
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) SetupCommPrefs(w http.ResponseWriter, r *http.Request, body apigen.SetupCommPrefsJSONRequestBody) {
	ctx := r.Context()
	emailAddress := swag.StringValue(body.Email)
	if emailAddress == "" {
		writeResponse(w, r, http.StatusBadRequest, "email is required")
		return
	}

	// validate email. if the user typed some value into the input, they might have a typo
	// we assume the intent was to provide a valid email, so we'll return an error
	if _, err := mail.ParseAddress(emailAddress); err != nil {
		c.Logger.WithError(err).WithField("email", emailAddress).Error("Setup comm prefs, invalid email address")
		writeError(w, r, http.StatusBadRequest, "invalid email address")
		return
	}

	// save comm prefs to metadata, for future in-app preferences/unsubscribe functionality
	commPrefs := auth.CommPrefs{
		UserEmail:       emailAddress,
		FeatureUpdates:  body.FeatureUpdates,
		SecurityUpdates: body.SecurityUpdates,
	}

	installationID, err := c.MetadataManager.UpdateCommPrefs(ctx, &commPrefs)
	if err != nil {
		c.Logger.WithError(err).WithField("email", emailAddress).Error("Setup comm prefs, failed to save comm prefs to metadata")
		writeError(w, r, http.StatusInternalServerError, err)
		return
	}

	// collect comm prefs
	go c.Collector.CollectCommPrefs(commPrefs.UserEmail, installationID, commPrefs.FeatureUpdates, commPrefs.SecurityUpdates)

	writeResponse(w, r, http.StatusOK, nil)
}

func (c *Controller) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	u, err := auth.GetUser(r.Context())
	var user apigen.User
	if err == nil {
		user.Id = u.Username
		user.CreationDate = u.CreatedAt.Unix()
		if u.FriendlyName != nil {
			user.FriendlyName = u.FriendlyName
		} else {
			user.FriendlyName = &u.Username
		}
	}
	response := apigen.CurrentUser{
		User: user,
	}
	writeResponse(w, r, http.StatusOK, response)
}

func (c *Controller) resetPasswordRequest(ctx context.Context, emailAddr string) error {
	user, err := c.Auth.GetUserByEmail(ctx, emailAddr)
	if err != nil {
		return err
	}
	emailAddr = apiutil.Value(user.Email)
	token, err := c.generateResetPasswordToken(emailAddr, DefaultResetPasswordExpiration)
	if err != nil {
		c.Logger.WithError(err).WithField("email_address", emailAddr).Error("reset password - failed generating token")
		return err
	}
	params := map[string]string{
		"token": token,
	}
	err = c.Emailer.SendResetPasswordEmail([]string{emailAddr}, params)
	if err != nil {
		c.Logger.WithError(err).WithField("email_address", emailAddr).Error("reset password - failed sending email")
		return err
	}
	c.Logger.WithField("email", emailAddr).Info("reset password email sent")
	return nil
}

func (c *Controller) ForgotPassword(w http.ResponseWriter, r *http.Request, body apigen.ForgotPasswordJSONRequestBody) {
	addr, err := mail.ParseAddress(body.Email)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid email")
		return
	}
	err = c.resetPasswordRequest(r.Context(), addr.Address)
	if err != nil {
		c.Logger.WithError(err).WithField("email", body.Email).Debug("failed sending reset password email")
	}
	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) UpdatePassword(w http.ResponseWriter, r *http.Request, body apigen.UpdatePasswordJSONRequestBody) {
	claims, err := VerifyResetPasswordToken(r.Context(), c.Auth, body.Token)
	if err != nil {
		c.Logger.WithError(err).WithField("token", body.Token).Debug("failed to verify token")
		writeError(w, r, http.StatusUnauthorized, ErrAuthenticatingRequest)
		return
	}

	// verify provided email matched the token
	requestEmail := apiutil.Value(body.Email)
	if requestEmail != "" && requestEmail != claims.Subject {
		c.Logger.WithError(err).WithFields(logging.Fields{
			"token":         body.Token,
			"request_email": requestEmail,
		}).Debug("requested email doesn't match the email provided in verified token")
	}

	user, err := c.Auth.GetUserByEmail(r.Context(), claims.Subject)
	if err != nil {
		c.Logger.WithError(err).WithField("email", claims.Subject).Warn("failed to retrieve user by email")
		writeError(w, r, http.StatusNotFound, http.StatusText(http.StatusNotFound))
		return
	}
	err = c.Auth.HashAndUpdatePassword(r.Context(), user.Username, body.NewPassword)
	if err != nil {
		c.Logger.WithError(err).WithField("username", user.Username).Debug("failed to update password")
		writeError(w, r, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return
	}
	writeResponse(w, r, http.StatusCreated, nil)
}

func (c *Controller) ExpandTemplate(w http.ResponseWriter, r *http.Request, templateLocation string, p apigen.ExpandTemplateParams) {
	if !c.authorize(w, r, permissions.Node{
		Permission: permissions.Permission{
			Action:   permissions.ReadObjectAction,
			Resource: permissions.TemplateArn(templateLocation),
		},
	}) {
		return
	}
	ctx := r.Context()

	u, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "request performed with no user")
	}

	// Override bug in OpenAPI generated code: parameters do not show up
	// in p.Params.AdditionalProperties, so force them in there.
	if len(p.Params.AdditionalProperties) == 0 && len(r.URL.Query()) > 0 {
		p.Params.AdditionalProperties = make(map[string]string, len(r.Header))
		for k, v := range r.URL.Query() {
			p.Params.AdditionalProperties[k] = v[0]
		}
	}

	c.LogAction(ctx, "expand_template", r, "", "", "")
	err = c.Templater.Expand(ctx, w, u, templateLocation, p.Params.AdditionalProperties)
	if err != nil {
		c.Logger.WithError(err).WithField("location", templateLocation).Error("Template expansion failed")
	}

	if errors.Is(err, templater.ErrNotAuthorized) {
		writeError(w, r, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		return
	}
	if errors.Is(err, templater.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, http.StatusText(http.StatusNotFound))
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "expansion failed")
		return
	}
	// Response already written during expansion.
}

func (c *Controller) GetLakeFSVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, ErrAuthenticatingRequest)
		return
	}

	// set upgrade recommended based on last security audit check
	var (
		upgradeRecommended *bool
		upgradeURL         *string
		latestVersion      *string
	)
	lastCheck, _ := c.AuditChecker.LastCheck()
	if lastCheck != nil {
		recommendedURL := lastCheck.UpgradeRecommendedURL()
		if recommendedURL != "" {
			upgradeRecommended = swag.Bool(true)
			upgradeURL = swag.String(recommendedURL)
		}
	}

	if c.Config.Security.CheckLatestVersion {
		latest, err := c.AuditChecker.CheckLatestVersion()
		// set upgrade recommended based on latest version
		if err != nil {
			c.Logger.WithError(err).Debug("failed to check latest version in releases")
		} else if latest != nil {
			latestVersion = swag.String(latest.LatestVersion)
			upgradeRecommended = swag.Bool(latest.Outdated || swag.BoolValue(upgradeRecommended))
			if latest.Outdated && upgradeURL == nil {
				upgradeURL = swag.String(version.DefaultReleasesURL)
			}
		}
	}

	writeResponse(w, r, http.StatusOK, apigen.VersionConfig{
		UpgradeRecommended: upgradeRecommended,
		UpgradeUrl:         upgradeURL,
		Version:            swag.String(version.Version),
		LatestVersion:      latestVersion,
	})
}

func (c *Controller) GetGarbageCollectionConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, ErrAuthenticatingRequest)
		return
	}

	writeResponse(w, r, http.StatusOK, apigen.GarbageCollectionConfig{
		GracePeriod: aws.Int(int(ref.LinkAddressTime.Seconds())),
	})
}

func (c *Controller) PostStatsEvents(w http.ResponseWriter, r *http.Request, body apigen.PostStatsEventsJSONRequestBody) {
	ctx := r.Context()
	user, err := auth.GetUser(ctx)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, ErrAuthenticatingRequest)
		return
	}

	for _, statsEv := range body.Events {
		if statsEv.Class == "" {
			writeError(w, r, http.StatusBadRequest, "invalid value: class is required")
			return
		}

		if statsEv.Name == "" {
			writeError(w, r, http.StatusBadRequest, "invalid value: name is required")
			return
		}

		if statsEv.Count < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid value: count must be a non-negative integer")
			return
		}
	}

	client := httputil.GetRequestLakeFSClient(r)
	for _, statsEv := range body.Events {
		ev := stats.Event{
			Class:  statsEv.Class,
			Name:   statsEv.Name,
			UserID: user.Username,
			Client: client,
		}
		c.Collector.CollectEvents(ev, uint64(statsEv.Count))

		c.Logger.WithContext(ctx).WithFields(logging.Fields{
			"class":   ev.Class,
			"name":    ev.Name,
			"count":   statsEv.Count,
			"user_id": ev.UserID,
			"client":  ev.Client,
		}).Debug("sending stats events")
	}

	writeResponse(w, r, http.StatusNoContent, nil)
}

func (c *Controller) OtfDiff(w http.ResponseWriter, r *http.Request, repository, leftRef, rightRef string, params apigen.OtfDiffParams) {
	ctx := r.Context()
	user, _ := auth.GetUser(ctx)
	c.LogAction(ctx, fmt.Sprintf("table_format_%s_diff", params.Type), r, repository, rightRef, leftRef)
	credentials, _, err := c.Auth.ListUserCredentials(ctx, user.Username, &model.PaginationParams{
		Prefix: "",
		After:  "",
		Amount: 1,
	})
	if c.handleAPIError(ctx, w, r, err) {
		return
	}
	if len(credentials) == 0 {
		writeError(w, r, http.StatusPreconditionFailed, "no programmatic credentials")
		return
	}

	baseCredential, err := c.Auth.GetCredentials(ctx, credentials[0].AccessKeyID)
	if c.handleAPIError(ctx, w, r, err) {
		return
	}

	listenAddress := c.Config.ListenAddress
	if strings.HasPrefix(listenAddress, ":") {
		// workaround in case we listen on all interfaces without specifying ip
		listenAddress = fmt.Sprintf("localhost%s", listenAddress)
	}

	tdp := tablediff.Params{
		// TODO(jonathan): add base RefPath
		TablePaths: tablediff.TablePaths{
			Left: tablediff.RefPath{
				Ref:  leftRef,
				Path: params.TablePath,
			},
			Right: tablediff.RefPath{
				Ref:  rightRef,
				Path: params.TablePath,
			},
		},
		S3Creds: tablediff.S3Creds{
			Key:      config.SecureString(baseCredential.AccessKeyID),
			Secret:   config.SecureString(baseCredential.SecretAccessKey),
			Endpoint: "http://" + listenAddress,
		},
		Repo: repository,
	}

	entries, err := c.otfDiffService.RunDiff(ctx, params.Type, tdp)
	if err != nil {
		c.Logger.WithError(err).
			WithContext(ctx).
			WithField("type", params.Type).
			WithField("table_diff_paths", fmt.Sprintf("%+v", tdp.TablePaths)).
			WithField("repo", tdp.Repo).
			Error("OTF Diff service failed")
		if errors.Is(err, tablediff.ErrTableNotFound) {
			writeError(w, r, http.StatusNotFound, err)
		} else {
			writeError(w, r, http.StatusInternalServerError, err)
		}
		return
	}
	writeResponse(w, r, http.StatusOK, buildOtfDiffListResponse(entries))
}

func (c *Controller) GetOtfDiffs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c.LogAction(ctx, "get_otf_diffs", r, "", "", "")
	diffTypes := c.otfDiffService.EnabledDiffs()
	diffs := make([]apigen.DiffProperties, 0, len(diffTypes))
	for _, diffType := range diffTypes {
		diffs = append(diffs, apigen.DiffProperties{
			Name: diffType,
		})
	}
	writeResponse(w, r, http.StatusOK, apigen.OTFDiffs{
		Diffs: &diffs,
	})
}

func buildOtfDiffListResponse(tableDiffResponse tablediff.Response) apigen.OtfDiffList {
	ol := make([]apigen.OtfDiffEntry, 0)
	for _, entry := range tableDiffResponse.Diffs {
		content := make(map[string]interface{})
		for k, v := range entry.OperationContent {
			content[k] = v
		}
		id := entry.ID
		ol = append(ol, apigen.OtfDiffEntry{
			Operation:        entry.Operation,
			OperationContent: content,
			Timestamp:        int(entry.Timestamp.Unix()),
			Id:               id,
			OperationType:    entry.OperationType,
		})
	}

	t := "changed"
	switch tableDiffResponse.DiffType {
	case tablediff.DiffTypeCreated:
		t = "created"
	case tablediff.DiffTypeDropped:
		t = "dropped"
	}
	return apigen.OtfDiffList{
		Results:  ol,
		DiffType: &t,
	}
}

func writeError(w http.ResponseWriter, r *http.Request, code int, v interface{}) {
	apiErr := apigen.Error{
		Message: fmt.Sprint(v),
	}
	writeResponse(w, r, code, apiErr)
}

func writeResponse(w http.ResponseWriter, r *http.Request, code int, response interface{}) {
	// check first if the client canceled the request
	if httputil.IsRequestCanceled(r) {
		w.WriteHeader(httpStatusClientClosedRequest) // Client closed request
		return
	}
	// nobody - just status code
	if response == nil {
		w.WriteHeader(code)
		return
	}
	// encode response body as json
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		logging.FromContext(r.Context()).WithError(err).WithField("code", code).Info("Failed to write encoded json response")
	}
}

func paginationAfter(v *apigen.PaginationAfter) string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func paginationPrefix(v *apigen.PaginationPrefix) string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func paginationDelimiter(v *apigen.PaginationDelimiter) string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func paginationAmount(v *apigen.PaginationAmount) int {
	if v == nil {
		return DefaultMaxPerPage
	}
	i := int(*v)
	if i > DefaultMaxPerPage {
		return DefaultMaxPerPage
	}
	if i <= 0 {
		return DefaultMaxPerPage
	}
	return i
}

func resolvePathList(objects, prefixes *[]string) []catalog.PathRecord {
	var pathRecords []catalog.PathRecord
	if objects == nil && prefixes == nil {
		return make([]catalog.PathRecord, 0)
	}
	if objects != nil {
		for _, path := range *objects {
			if path != "" {
				path := path
				pathRecords = append(pathRecords, catalog.PathRecord{
					Path:     catalog.Path(apiutil.Value(&path)),
					IsPrefix: false,
				})
			}
		}
	}
	if prefixes != nil {
		for _, path := range *prefixes {
			if path != "" {
				path := path
				pathRecords = append(pathRecords, catalog.PathRecord{
					Path:     catalog.Path(apiutil.Value(&path)),
					IsPrefix: true,
				})
			}
		}
	}
	return pathRecords
}

func NewController(
	cfg *config.Config,
	catalog catalog.Interface,
	authenticator auth.Authenticator,
	authService auth.Service,
	blockAdapter block.Adapter,
	metadataManager auth.MetadataManager,
	migrator Migrator,
	collector stats.Collector,
	cloudMetadataProvider cloud.MetadataProvider,
	actions actionsHandler,
	auditChecker AuditChecker,
	logger logging.Logger,
	emailer *email.Emailer,
	templater templater.Service,
	sessionStore sessions.Store,
	pathProvider upload.PathProvider,
	otfDiffService *tablediff.Service,
) *Controller {
	return &Controller{
		Config:                cfg,
		Catalog:               catalog,
		Authenticator:         authenticator,
		Auth:                  authService,
		BlockAdapter:          blockAdapter,
		MetadataManager:       metadataManager,
		Migrator:              migrator,
		Collector:             collector,
		CloudMetadataProvider: cloudMetadataProvider,
		Actions:               actions,
		AuditChecker:          auditChecker,
		Logger:                logger,
		Emailer:               emailer,
		Templater:             templater,
		sessionStore:          sessionStore,
		PathProvider:          pathProvider,
		otfDiffService:        otfDiffService,
	}
}

func (c *Controller) LogAction(ctx context.Context, action string, r *http.Request, repository, ref, sourceRef string) {
	client := httputil.GetRequestLakeFSClient(r)
	ev := stats.Event{
		Class:      "api_server",
		Name:       action,
		Repository: repository,
		Ref:        ref,
		SourceRef:  sourceRef,
		Client:     client,
	}
	user, _ := auth.GetUser(ctx)
	if user != nil {
		ev.UserID = user.Username
	}

	sourceIP := httputil.SourceIP(r)

	c.Logger.WithContext(ctx).WithFields(logging.Fields{
		"class":      ev.Class,
		"name":       ev.Name,
		"repository": ev.Repository,
		"ref":        ev.Ref,
		"source_ref": ev.SourceRef,
		"user_id":    ev.UserID,
		"client":     ev.Client,
		"source_ip":  sourceIP,
	}).Debug("performing API action")
	c.Collector.CollectEvent(ev)
}

func paginationFor(hasMore bool, results interface{}, fieldName string) apigen.Pagination {
	pagination := apigen.Pagination{
		HasMore:    hasMore,
		MaxPerPage: DefaultMaxPerPage,
	}
	if results == nil {
		return pagination
	}
	if reflect.TypeOf(results).Kind() != reflect.Slice {
		panic("results is not a slice")
	}
	s := reflect.ValueOf(results)
	pagination.Results = s.Len()
	if !hasMore || pagination.Results == 0 {
		return pagination
	}
	v := s.Index(pagination.Results - 1)
	token := v.FieldByName(fieldName)
	pagination.NextOffset = token.String()
	return pagination
}

func (c *Controller) authorizeCallback(w http.ResponseWriter, r *http.Request, perms permissions.Node, cb func(w http.ResponseWriter, r *http.Request, code int, v interface{})) bool {
	ctx := r.Context()
	user, err := auth.GetUser(ctx)
	if err != nil {
		cb(w, r, http.StatusUnauthorized, ErrAuthenticatingRequest)
		return false
	}
	resp, err := c.Auth.Authorize(ctx, &auth.AuthorizationRequest{
		Username:            user.Username,
		RequiredPermissions: perms,
	})
	if err != nil {
		cb(w, r, http.StatusInternalServerError, err)
		return false
	}
	if resp.Error != nil {
		cb(w, r, http.StatusUnauthorized, resp.Error)
		return false
	}
	if !resp.Allowed {
		cb(w, r, http.StatusInternalServerError, "User does not have the required permissions")
		return false
	}
	return true
}

func (c *Controller) authorize(w http.ResponseWriter, r *http.Request, perms permissions.Node) bool {
	return c.authorizeCallback(w, r, perms, writeError)
}

func (c *Controller) isNameValid(name, nameType string) (bool, string) {
	// URLs are % encoded. Allowing % signs in entity names would
	// limit the ability to use these entity names in the URL for both
	// client-side routing and API fetch requests
	if strings.Contains(name, "%") {
		return false, fmt.Sprintf("%s name cannot contain '%%'", nameType)
	}
	return true, ""
}

func decodeGCUncommittedMark(mark string) (*catalog.GCUncommittedMark, error) {
	if mark == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(mark)
	if err != nil {
		return nil, err
	}
	var result catalog.GCUncommittedMark
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func encodeGCUncommittedMark(mark *catalog.GCUncommittedMark) (*string, error) {
	if mark == nil {
		return nil, nil
	}
	raw, err := json.Marshal(mark)
	if err != nil {
		return nil, err
	}

	token := base64.StdEncoding.EncodeToString(raw)
	return &token, nil
}

func extractLakeFSMetadata(header http.Header) map[string]string {
	meta := make(map[string]string)
	for k, v := range header {
		lowerKey := strings.ToLower(k)
		metaKey := ""
		switch {
		case strings.HasPrefix(lowerKey, apiutil.LakeFSHeaderMetadataPrefix):
			metaKey = lowerKey[len(apiutil.LakeFSHeaderMetadataPrefix):]
		case strings.HasPrefix(lowerKey, apiutil.LakeFSHeaderInternalPrefix):
			metaKey = apiutil.LakeFSMetadataPrefix + lowerKey[len(apiutil.LakeFSHeaderInternalPrefix):]
		default:
			continue
		}
		meta[metaKey] = v[0]
	}
	return meta
}
