package controller

import (
	"fmt"
	"time"

	"github.com/almighty/almighty-core/account"
	"github.com/almighty/almighty-core/app"
	"github.com/almighty/almighty-core/application"
	"github.com/almighty/almighty-core/errors"
	"github.com/almighty/almighty-core/jsonapi"
	"github.com/almighty/almighty-core/log"
	"github.com/almighty/almighty-core/login"
	"github.com/almighty/almighty-core/rest"
	"github.com/almighty/almighty-core/workitem"
	"github.com/goadesign/goa"
	errs "github.com/pkg/errors"

	uuid "github.com/satori/go.uuid"
)

// UsersController implements the users resource.
type UsersController struct {
	*goa.Controller
	db     application.DB
	config UsersControllerConfiguration
}

// UsersControllerConfiguration the configuration for the UsersController
type UsersControllerConfiguration interface {
	GetCacheControlUsers() string
}

// NewUsersController creates a users controller.
func NewUsersController(service *goa.Service, db application.DB, config UsersControllerConfiguration) *UsersController {
	return &UsersController{
		Controller: service.NewController("UsersController"),
		db:         db,
		config:     config,
	}
}

// Show runs the show action.
func (c *UsersController) Show(ctx *app.ShowUsersContext) error {
	return application.Transactional(c.db, func(appl application.Application) error {
		identityID, err := uuid.FromString(ctx.ID)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, errs.Wrap(errors.NewBadParameterError("identity_id", ctx.ID), err.Error()))
		}
		identity, err := appl.Identities().Load(ctx.Context, identityID)
		if err != nil {
			jerrors, httpStatusCode := jsonapi.ErrorToJSONAPIErrors(err)
			return ctx.ResponseData.Service.Send(ctx.Context, httpStatusCode, jerrors)
		}
		var user *account.User
		userID := identity.UserID
		if userID.Valid {
			user, err = appl.Users().Load(ctx.Context, userID.UUID)
			if err != nil {
				return jsonapi.JSONErrorResponse(ctx, errors.NewBadParameterError(fmt.Sprintf("User ID %s not valid", userID.UUID), err))
			}
		}
		return ctx.ConditionalEntity(*user, c.config.GetCacheControlUsers, func() error {
			return ctx.OK(ConvertToAppUser(ctx.RequestData, user, identity))
		})
	})
}

// Update updates the authorized user based on the provided Token
func (c *UsersController) Update(ctx *app.UpdateUsersContext) error {

	id, err := login.ContextIdentity(ctx)
	if err != nil {
		return jsonapi.JSONErrorResponse(ctx, goa.ErrUnauthorized(err.Error()))
	}

	return application.Transactional(c.db, func(appl application.Application) error {
		identity, err := appl.Identities().Load(ctx, *id)
		if err != nil || identity == nil {
			log.Error(ctx, map[string]interface{}{
				"identity_id": id,
			}, "auth token contains id %s of unknown Identity", *id)
			jerrors, _ := jsonapi.ErrorToJSONAPIErrors(goa.ErrUnauthorized(fmt.Sprintf("Auth token contains id %s of unknown Identity\n", *id)))
			return ctx.Unauthorized(jerrors)
		}

		var user *account.User
		if identity.UserID.Valid {
			user, err = appl.Users().Load(ctx.Context, identity.UserID.UUID)
			if err != nil {
				return jsonapi.JSONErrorResponse(ctx, errs.Wrap(err, fmt.Sprintf("Can't load user with id %s", identity.UserID.UUID)))
			}
		}

		updatedEmail := ctx.Payload.Data.Attributes.Email
		if updatedEmail != nil {
			user.Email = *updatedEmail
		}
		updatedBio := ctx.Payload.Data.Attributes.Bio
		if updatedBio != nil {
			user.Bio = *updatedBio
		}
		updatedFullName := ctx.Payload.Data.Attributes.FullName
		if updatedFullName != nil {
			user.FullName = *updatedFullName
		}
		updatedImageURL := ctx.Payload.Data.Attributes.ImageURL
		if updatedImageURL != nil {
			user.ImageURL = *updatedImageURL
		}
		updateURL := ctx.Payload.Data.Attributes.URL
		if updateURL != nil {
			user.URL = *updateURL
		}

		updatedContextInformation := ctx.Payload.Data.Attributes.ContextInformation
		if updatedContextInformation != nil {
			// if user.ContextInformation , we get to PATCH the ContextInformation field,
			// instead of over-writing it altogether. Note: The PATCH-ing is only for the
			// 1st level of JSON.
			if user.ContextInformation == nil {
				user.ContextInformation = workitem.Fields{}
			}
			for fieldName, fieldValue := range updatedContextInformation {
				// Save it as is, for short-term.
				user.ContextInformation[fieldName] = fieldValue
			}
		}

		err = appl.Users().Save(ctx, user)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, err)
		}

		err = appl.Identities().Save(ctx, identity)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, err)
		}

		return ctx.OK(ConvertToAppUser(ctx.RequestData, user, identity))
	})
}

// List runs the list action.
func (c *UsersController) List(ctx *app.ListUsersContext) error {
	return application.Transactional(c.db, func(appl application.Application) error {
		var err error
		var users []account.User
		users, err = appl.Users().List(ctx.Context)
		if err != nil {
			return jsonapi.JSONErrorResponse(ctx, errs.Wrap(err, "Error listing users"))
		}
		return ctx.ConditionalEntities(users, c.config.GetCacheControlUsers, func() error {
			result, err := LoadKeyCloakIdentities(appl, ctx.RequestData, users)
			if err != nil {
				return jsonapi.JSONErrorResponse(ctx, errs.Wrap(err, "Error listing users"))
			}
			return ctx.OK(result)
		})
	})
}

// LoadKeyCloakIdentities loads keycloak identies for the users and converts the users into REST representation
func LoadKeyCloakIdentities(appl application.Application, request *goa.RequestData, users []account.User) (*app.UserArray, error) {
	data := make([]*app.UserData, len(users))
	for i, user := range users {
		identity, err := loadKeyCloakIdentity(appl, user)
		if err != nil {
			return nil, err
		}
		appUser := ConvertToAppUser(request, &user, identity)
		data[i] = appUser.Data
	}
	return &app.UserArray{Data: data}, nil
}

func loadKeyCloakIdentity(appl application.Application, user account.User) (*account.Identity, error) {
	identities, err := appl.Identities().Query(account.IdentityFilterByUserID(user.ID))
	if err != nil {
		return nil, err
	}
	for _, identity := range identities {
		if identity.ProviderType == account.KeycloakIDP {
			return &identity, nil
		}
	}
	return nil, fmt.Errorf("Can't find Keycloak Identity for user %s", user.Email)
}

// ConvertToAppUser converts a complete Identity object into REST representation
func ConvertToAppUser(request *goa.RequestData, user *account.User, identity *account.Identity) *app.User {
	userID := user.ID.String()
	fullName := user.FullName
	userName := identity.Username
	providerType := identity.ProviderType
	identityID := identity.ID.String()
	var imageURL string
	var bio string
	var userURL string
	var email string
	var createdAt time.Time
	var updatedAt time.Time
	var contextInformation workitem.Fields

	if user != nil {
		fullName = user.FullName
		imageURL = user.ImageURL
		bio = user.Bio
		userURL = user.URL
		email = user.Email
		contextInformation = user.ContextInformation
		// CreatedAt and UpdatedAt fields in the resulting app.Identity are based on the 'user' entity
		createdAt = user.CreatedAt
		updatedAt = user.UpdatedAt
	}

	// The following will be used for ContextInformation.
	// The simplest way to represent is to have all fields
	// as a SimpleType. During conversion from 'model' to 'app',
	// the value would be returned 'as is'.

	simpleFieldDefinition := workitem.FieldDefinition{
		Type: workitem.SimpleType{Kind: workitem.KindString},
	}

	converted := app.User{
		Data: &app.UserData{
			ID:   &userID,
			Type: "users",
			Attributes: &app.UserDataAttributes{
				Username:           &userName,
				FullName:           &fullName,
				ImageURL:           &imageURL,
				Bio:                &bio,
				URL:                &userURL,
				IdentityID:         &identityID,
				ProviderType:       &providerType,
				Email:              &email,
				ContextInformation: workitem.Fields{},
				CreatedAt:          &createdAt,
				UpdatedAt:          &updatedAt,
			},
			Links: createUserLinks(request, &identity.ID),
		},
	}
	for name, value := range contextInformation {
		if value == nil {
			// this can be used to unset a key in contextInformation
			continue
		}
		convertedValue, err := simpleFieldDefinition.ConvertFromModel(name, value)
		if err != nil {
			log.Error(nil, map[string]interface{}{
				"err": err,
			}, "Unable to convert user context field %s ", name)
			converted.Data.Attributes.ContextInformation[name] = nil
		}
		converted.Data.Attributes.ContextInformation[name] = convertedValue
	}
	return &converted
}

// ConvertUsersSimple converts a array of simple Identity IDs into a Generic Reletionship List
func ConvertUsersSimple(request *goa.RequestData, identityIDs []interface{}) []*app.GenericData {
	ops := []*app.GenericData{}
	for _, identityID := range identityIDs {
		ops = append(ops, ConvertUserSimple(request, identityID))
	}
	return ops
}

// ConvertUserSimple converts a simple Identity ID into a Generic Reletionship
func ConvertUserSimple(request *goa.RequestData, identityID interface{}) *app.GenericData {
	t := "users"
	i := fmt.Sprint(identityID)
	return &app.GenericData{
		Type:  &t,
		ID:    &i,
		Links: createUserLinks(request, identityID),
	}
}

func createUserLinks(request *goa.RequestData, identityID interface{}) *app.GenericLinks {
	selfURL := rest.AbsoluteURL(request, app.UsersHref(identityID))
	return &app.GenericLinks{
		Self: &selfURL,
	}
}
