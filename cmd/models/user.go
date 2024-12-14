package models

import (
	"time"

	"gorm.io/gorm"
)


type User struct {
    gorm.Model
    FullName       string    `gorm:"column:full_name;size:255;not null" json:"full_name"`
    Email          string    `gorm:"column:email;size:255;uniqueIndex;not null" json:"email"`
    PasswordHash   string    `gorm:"column:password_hash;size:255;not null" json:"password_hash"`
    Role           string    `gorm:"column:role;size:50;not null" json:"role"`
    Phone          string    `gorm:"column:phone;size:20;unique" json:"phone"`
    PhoneVerified  bool      `gorm:"column:phone_verified;default:false" json:"phone_verified"`
    EmailVerified   bool      `gorm:"column:email_verified;default:false" json:"email_verified"`
    OtpCode        string    `gorm:"column:otp_code;size:10" json:"otp_code"`
    Status         string    `gorm:"column:status;size:50;not null;default:inactive" json:"status"`
    Refresh        string    `gorm:"column:refresh_token;size:255" json:"refresh_token"`
    RefreshTokenExpiredAt time.Time `gorm:"column:refresh_token_expired_at" json:"refresh_token_expired_at"`
    ProfilePicturePath string `gorm:"column:profile_picture_path;size:255" json:"profile_picture_path"`
    EmailVerificationCode string    `gorm:"size:6"`
    VerificationExpiry    time.Time `gorm:""`

    Expert         *Expert   `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE;nullable" json:"expert,omitempty"`
}


type Expert struct {
    gorm.Model
    UserID         uint      `gorm:"column:user_id;not null" json:"user_id"`
    Expertise      string    `gorm:"column:expertise;size:255" json:"expertise"`
    Bio            string    `gorm:"column:bio;type:text" json:"bio"`
    Verified       bool      `gorm:"column:verified;default:false" json:"verified"`
    CertificationFiles []CertificationFile `gorm:"foreignKey:ExpertID;constraint:OnDelete:CASCADE;" json:"certification_files"` 
    User           *User     `gorm:"foreignKey:UserID" json:"-"`
}


type CertificationFile struct {
    gorm.Model
    ExpertID uint   `gorm:"column:expert_id;not null" json:"expert_id"` 
    FileName string `gorm:"column:file_name;size:255;not null" json:"file_name"`
    FilePath string `gorm:"column:file_path;size:500;not null" json:"file_path"`
}


type PasswordResetToken struct {
    ID        uint      `gorm:"primaryKey"`
    UserID    uint      `gorm:"not null"`
    Token     string    `gorm:"not null"`
    ExpiresAt time.Time `gorm:"not null"`
}


func (Expert) TableName() string {
    return "experts"
}