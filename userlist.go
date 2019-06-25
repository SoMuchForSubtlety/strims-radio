package main

import "sync"

type userList struct {
	Users []string
	sync.Mutex
}

func (u *userList) add(user string) {
	u.remove(user)
	u.Lock()
	u.Users = append(u.Users, user)
	u.Unlock()
}

func (u *userList) remove(user string) {
	u.Lock()
	index := u.search(user)
	if index >= 0 {
		u.Users = append(u.Users[:index], u.Users[index+1:]...)
	}
	u.Unlock()
}

func (u *userList) search(nick string) int {
	for i, user := range u.Users {
		if user == nick {
			return i
		}
	}
	return -1
}

func (u *userList) clear() {
	u.Lock()
	u.Users = nil
	u.Unlock()
}
