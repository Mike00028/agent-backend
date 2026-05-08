drop trigger if exists trg_sync_conversation_on_message on public.messages;
drop trigger if exists trg_set_message_sequence on public.messages;
drop function if exists public.sync_conversation_on_message();
drop function if exists public.set_message_sequence();
drop table if exists public.messages;
drop table if exists public.conversations;
