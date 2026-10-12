package service

// DropStoresForTest forgets opened datastores so the next use re-opens them (tests).
func (s *Service) DropStoresForTest() { s.dropStores() }
