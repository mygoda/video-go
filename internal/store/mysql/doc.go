// Package mysql holds the MySQL implementations of the repository interfaces
// declared in internal/store.
//
// 除本包外，任何地方都不该出现 SQL。上层拿到的是 store 里的接口，
// 换存储只需要换掉本包。
//
// 表结构由 ./migrations 下的迁移文件定义（由另一条工作线维护），
// 本包按那份 schema 实现读写。
package mysql
