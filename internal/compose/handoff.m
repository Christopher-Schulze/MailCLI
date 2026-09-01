#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#import <stdlib.h>
#import <string.h>

#import "handoff.h"

@interface MailCLISharingDelegate : NSObject <NSSharingServiceDelegate>
@property(nonatomic, assign) BOOL completed;
@property(nonatomic, assign) BOOL succeeded;
@property(nonatomic, copy) NSString *failureMessage;
@end

@implementation MailCLISharingDelegate

- (void)sharingService:(NSSharingService *)sharingService didShareItems:(NSArray *)items {
    self.succeeded = YES;
    self.completed = YES;
}

- (void)sharingService:(NSSharingService *)sharingService
    didFailToShareItems:(NSArray *)items
                  error:(NSError *)error {
    self.failureMessage = error.localizedDescription ?: @"macOS could not open the compose window";
    self.completed = YES;
}

@end

static char *mailcli_response(BOOL ok, NSString *code, NSString *message, BOOL opened) {
    NSDictionary *response = @{
        @"ok": @(ok),
        @"code": code ?: @"",
        @"message": message ?: @"",
        @"opened": @(opened),
        @"mail_application": ok ? @"com.apple.mail" : @""
    };
    NSError *error = nil;
    NSData *data = [NSJSONSerialization dataWithJSONObject:response options:0 error:&error];
    if (data == nil) {
        return strdup("{\"ok\":false,\"code\":\"handoff_failed\",\"message\":\"could not encode native handoff result\"}");
    }
    NSString *json = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
    return strdup(json.UTF8String);
}

static NSString *mailcli_default_email_bundle_identifier(void) {
    NSURL *mailtoURL = [NSURL URLWithString:@"mailto:mailcli@example.invalid"];
    NSURL *applicationURL = [[NSWorkspace sharedWorkspace] URLForApplicationToOpenURL:mailtoURL];
    if (applicationURL == nil) {
        return nil;
    }
    NSBundle *bundle = [NSBundle bundleWithURL:applicationURL];
    return bundle.bundleIdentifier;
}

char *mailcli_compose_email(const char *request_json) {
    @autoreleasepool {
        if (![NSThread isMainThread]) {
            return mailcli_response(NO, @"handoff_thread_invalid", @"visible compose must run on the macOS main thread", NO);
        }
        if (request_json == NULL) {
            return mailcli_response(NO, @"invalid_argument", @"compose handoff request is missing", NO);
        }
        NSData *payload = [NSData dataWithBytes:request_json length:strlen(request_json)];
        NSError *decodeError = nil;
        id decoded = [NSJSONSerialization JSONObjectWithData:payload options:0 error:&decodeError];
        if (![decoded isKindOfClass:[NSDictionary class]]) {
            return mailcli_response(NO, @"invalid_argument", @"compose handoff request is invalid", NO);
        }
        NSDictionary *request = (NSDictionary *)decoded;
        NSString *defaultBundleIdentifier = mailcli_default_email_bundle_identifier();
        if (![defaultBundleIdentifier isEqualToString:@"com.apple.mail"]) {
            NSString *actual = defaultBundleIdentifier ?: @"none";
            NSString *message = [NSString stringWithFormat:
                @"Mail.app is not the default email application (current handler: %@); select Mail in Mail > Settings > General > Default email reader",
                actual];
            return mailcli_response(NO, @"default_mail_app_required", message, NO);
        }

        NSArray *recipientValues = request[@"recipients"];
        NSString *subject = request[@"subject"];
        NSString *plainBody = request[@"plain_body"];
        NSString *htmlBody = request[@"html_body"];
        NSArray *attachmentPaths = request[@"attachments"];
        if (![recipientValues isKindOfClass:[NSArray class]] || ![subject isKindOfClass:[NSString class]] ||
            ![plainBody isKindOfClass:[NSString class]] || ![attachmentPaths isKindOfClass:[NSArray class]]) {
            return mailcli_response(NO, @"invalid_argument", @"compose handoff fields are invalid", NO);
        }

        NSMutableArray *items = [NSMutableArray arrayWithCapacity:attachmentPaths.count + 1];
        if ([htmlBody isKindOfClass:[NSString class]] && htmlBody.length > 0) {
            NSData *htmlData = [htmlBody dataUsingEncoding:NSUTF8StringEncoding];
            NSDictionary *options = @{
                NSDocumentTypeDocumentOption: NSHTMLTextDocumentType,
                NSCharacterEncodingDocumentOption: @(NSUTF8StringEncoding)
            };
            NSError *htmlError = nil;
            NSAttributedString *attributedBody = [[NSAttributedString alloc]
                initWithData:htmlData options:options documentAttributes:nil error:&htmlError];
            if (attributedBody == nil) {
                return mailcli_response(NO, @"invalid_body", @"could not materialize the validated HTML body", NO);
            }
            [items addObject:attributedBody];
        } else {
            [items addObject:plainBody];
        }
        for (id value in attachmentPaths) {
            if (![value isKindOfClass:[NSString class]]) {
                return mailcli_response(NO, @"invalid_attachment", @"attachment path is invalid", NO);
            }
            NSURL *url = [NSURL fileURLWithPath:(NSString *)value isDirectory:NO];
            [items addObject:url];
        }

        NSSharingService *service = [NSSharingService sharingServiceNamed:NSSharingServiceNameComposeEmail];
        if (service == nil || ![service canPerformWithItems:items]) {
            return mailcli_response(NO, @"compose_service_unavailable", @"macOS Compose Email sharing service is unavailable for this draft", NO);
        }
        [NSApplication sharedApplication];
        MailCLISharingDelegate *delegate = [[MailCLISharingDelegate alloc] init];
        service.delegate = delegate;
        service.recipients = recipientValues;
        service.subject = subject;
        [service performWithItems:items];

        NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:10.0];
        while (!delegate.completed && deadline.timeIntervalSinceNow > 0) {
            NSDate *slice = [NSDate dateWithTimeIntervalSinceNow:0.05];
            [[NSRunLoop currentRunLoop] runMode:NSDefaultRunLoopMode beforeDate:slice];
        }
        if (!delegate.completed) {
            return mailcli_response(NO, @"handoff_outcome_unknown", @"macOS did not confirm that the compose window opened", NO);
        }
        if (!delegate.succeeded) {
            return mailcli_response(NO, @"handoff_failed", delegate.failureMessage, NO);
        }
        return mailcli_response(YES, nil, nil, YES);
    }
}
