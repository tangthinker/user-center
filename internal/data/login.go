package data

type LoginReq struct {
	Uid      string `json:"uid"`
	Password string `json:"password"`
}

type LoginResp struct {
	Token string `json:"token"`
}

type RegisterReq struct {
	Uid      string `json:"uid"`
	Password string `json:"password"`
}

type RegisterResp struct {
}

type ModifyPasswordReq struct {
	Uid         string `json:"uid"`
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

type ModifyPasswordResp struct {
}

type UidUniqueReq struct {
	Uid string `json:"uid"`
}

type VerifyReq struct {
	Token string `json:"token"`
}
